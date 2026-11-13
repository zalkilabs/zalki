package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
)

func promptQuotaActor(index int) string {
	return fmt.Sprintf("cursor:00000000-0000-4000-8000-%012d", index)
}

func TestPromptQuotaPerActorWindowAndRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	actor := promptQuotaActor(1)
	if err := debitPromptRegistration(dir, actor, now, journal.WriteAtomic); err != nil {
		t.Fatal(err)
	}
	for _, elapsed := range []time.Duration{0, time.Second, promptQuotaWindow - time.Nanosecond} {
		if err := debitPromptRegistration(dir, actor, now.Add(elapsed), journal.WriteAtomic); !errors.Is(err, errPromptQuota) {
			t.Fatal("C06_PER_ID_PERSISTED_WINDOW", elapsed, err)
		}
	}
	if err := debitPromptRegistration(dir, actor, now.Add(promptQuotaWindow), journal.WriteAtomic); err != nil {
		t.Fatal("C06_EXACT_EXPIRY", err)
	}
	quota, err := readPromptQuota(filepath.Join(dir, "hook-registration-quota.json"))
	if err != nil || len(quota.Attempts) != 1 || quota.Attempts[0].At != now.Add(promptQuotaWindow).Format(time.RFC3339Nano) {
		t.Fatal("C06_RESTART_READS_DISK", err, quota)
	}
}

func TestPromptQuotaGlobalWindow(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for index := range promptQuotaCount {
		if err := debitPromptRegistration(dir, promptQuotaActor(index), now, journal.WriteAtomic); err != nil {
			t.Fatal("C07_TWENTY_ATTEMPTS", index, err)
		}
	}
	path := filepath.Join(dir, "hook-registration-quota.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := debitPromptRegistration(dir, promptQuotaActor(21), now.Add(promptQuotaWindow-time.Nanosecond), journal.WriteAtomic); !errors.Is(err, errPromptQuota) {
		t.Fatal("C07_GLOBAL_CAP", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("C07_DENIAL_DOES_NOT_EXTEND", err)
	}
	if err := debitPromptRegistration(dir, promptQuotaActor(21), now.Add(promptQuotaWindow), journal.WriteAtomic); err != nil {
		t.Fatal("C07_GLOBAL_EXPIRY", err)
	}
}

func TestPromptQuotaRejectsUnsafeCompleteLedger(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	valid := `{"schema":1,"attempts":[{"actor":"` + promptQuotaActor(1) + `","at":"2026-11-12T12:00:00Z"}]}`
	for name, raw := range map[string]string{
		"corrupt": "{", "oversize": `{"schema":1,"attempts":[]}` + strings.Repeat(" ", promptQuotaFileLimit),
		"schema": `{"schema":2,"attempts":[]}`, "missing": `{"schema":1}`,
		"duplicate top":     `{"schema":1,"schema":1,"attempts":[]}`,
		"escaped duplicate": `{"schema":1,"sc\u0068ema":1,"attempts":[]}`,
		"null attempts":     `{"schema":1,"attempts":null}`, "unknown": `{"schema":1,"attempts":[],"x":1}`,
		"future":              strings.Replace(valid, "12:00:00Z", "12:00:00.000000001Z", 1),
		"offset":              strings.Replace(valid, "12:00:00Z", "12:00:00+00:00", 1),
		"zero":                strings.Replace(valid, "2026-11-12T12:00:00Z", "0001-01-01T00:00:00Z", 1),
		"invalid actor":       strings.Replace(valid, promptQuotaActor(1), "../path", 1),
		"duplicate row field": strings.Replace(valid, `"at":`, `"actor":"`+promptQuotaActor(2)+`","at":`, 1),
		"invalid UTF8":        strings.Replace(valid, "cursor:", "\xff:", 1),
	} {
		t.Run(name, func(t *testing.T) { assertPromptQuotaRefusal(t, raw, now) })
	}
	quota := promptQuota{Schema: 1, Attempts: []promptQuotaAttempt{{Actor: promptQuotaActor(1), At: "2026-11-12T11:00:00Z"}, {Actor: promptQuotaActor(1), At: "2026-11-12T11:00:00Z"}}}
	raw, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	assertPromptQuotaRefusal(t, string(raw), now)
}

type promptQuotaInfo struct {
	os.FileInfo
	stat *syscall.Stat_t
	mode os.FileMode
}

func (i promptQuotaInfo) Sys() any          { return i.stat }
func (i promptQuotaInfo) Mode() os.FileMode { return i.mode }

func TestPromptQuotaPrivateFilePredicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := *info.Sys().(*syscall.Stat_t)
	for _, test := range []struct {
		mode os.FileMode
		uid  int
		want bool
	}{
		{0600, os.Getuid(), true}, {0644, os.Getuid(), false}, {0600, os.Getuid() + 1, false},
		{os.ModeDir | 0700, os.Getuid(), false}, {os.ModeSymlink | 0600, os.Getuid(), false},
	} {
		if privatePromptQuotaFile(promptQuotaInfo{FileInfo: info, stat: &stat, mode: test.mode}, test.uid) != test.want {
			t.Fatal("C09_PRIVATE_QUOTA_FILE_TYPE_MODE_AND_OWNER", test.mode, test.uid)
		}
	}
}

func assertPromptQuotaRefusal(t *testing.T, raw string, now time.Time) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hook-registration-quota.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	err := debitPromptRegistration(dir, promptQuotaActor(9), now, func(string, []byte) error {
		t.Fatal("C09_UNSAFE_STATE_WAS_REWRITTEN")
		return nil
	})
	if !errors.Is(err, errPromptQuotaState) {
		t.Fatal("C09_UNSAFE_LEDGER_REFUSED", err)
	}
}

func TestPromptQuotaRejectsUnsafeFile(t *testing.T) {
	for _, mode := range []string{"symlink", "directory", "public mode"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "hook-registration-quota.json")
			var err error
			switch mode {
			case "symlink":
				err = os.Symlink(filepath.Join(dir, "missing"), path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "public mode":
				err = os.WriteFile(path, []byte(`{"schema":1,"attempts":[]}`), 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := debitPromptRegistration(dir, promptQuotaActor(1), time.Now().UTC(), journal.WriteAtomic); !errors.Is(err, errPromptQuotaState) {
				t.Fatal("C09_UNSAFE_FILE_REFUSED", err)
			}
		})
	}
}
