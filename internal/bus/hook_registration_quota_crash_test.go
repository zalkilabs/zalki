package bus

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
)

func TestPromptQuotaProcessCrashPublicationCuts(t *testing.T) {
	if dir := os.Getenv("ZALKI_QUOTA_CUT_STATE"); dir != "" {
		runPromptQuotaCrashCut(t, dir, os.Getenv("ZALKI_QUOTA_CUT_NATIVE"), os.Getenv("ZALKI_QUOTA_CUT_STAGE"))
		return
	}
	for _, cut := range []string{"temporary-synced", "renamed", "directory-synced"} {
		t.Run(cut, func(t *testing.T) { assertPromptQuotaCrashCut(t, cut) })
	}
}

func assertPromptQuotaCrashCut(t *testing.T, cut string) {
	t.Helper()
	store := fixtureStore(t)
	native := newID()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPromptQuotaProcessCrashPublicationCuts$", "-test.count=1")
	child.Env = append(os.Environ(), "ZALKI_QUOTA_CUT_STATE="+store.Dir, "ZALKI_QUOTA_CUT_NATIVE="+native, "ZALKI_QUOTA_CUT_STAGE="+cut)
	err := child.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatal("C18_ACTUAL_QUOTA_CRASH_CUT", cut, err)
	}
	if _, err := store.session("cursor:" + native); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("C18_PRE_WAL_CRASH_CREATED_NO_SESSION", err)
	}
	_, err = prepareHookRuntime(store, "cursor", "cursor:"+native, trustedPrompt(native))
	if cut == "temporary-synced" {
		if err != nil {
			t.Fatal("C18_UNPUBLISHED_TEMPORARY_DEBIT_DOES_NOT_BLOCK_RESTART", err)
		}
	} else if !errors.Is(err, errPromptQuota) {
		t.Fatal("C18_PUBLISHED_DEBIT_REMAINS_SPENT_AFTER_PROCESS_EXIT", cut, err)
	}
	quota, err := readPromptQuota(filepath.Join(store.Dir, "hook-registration-quota.json"))
	if err != nil || len(quota.Attempts) != 1 {
		t.Fatal("C18_CRASH_RECOVERY_ACCOUNTING_EXACTLY_ONE", err)
	}
}

func runPromptQuotaCrashCut(t *testing.T, dir, native, cut string) {
	t.Helper()
	store := Store{Dir: dir, Registry: testRegistry()}
	err := store.locked(func() error {
		_, err := registerPromptAbsent(store, "cursor", "cursor:"+native, trustedPrompt(native), time.Now().UTC(), func(path string, raw []byte) error { return crashPromptQuotaPublication(path, raw, cut) })
		return err
	})
	t.Fatal("C18_QUOTA_CRASH_POINT_NOT_REACHED", err)
}

// Real process exits leave the same files as a crash at each atomic-write
// boundary. This models process loss, not a machine/power-loss experiment.
func crashPromptQuotaPublication(path string, raw []byte, cut string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".prompt-quota-crash-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	exitPromptQuotaAt(cut, "temporary-synced")
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	exitPromptQuotaAt(cut, "renamed")
	if err := journal.SyncDir(filepath.Dir(path)); err != nil {
		return err
	}
	exitPromptQuotaAt(cut, "directory-synced")
	return errors.New("unknown quota crash cut")
}

func exitPromptQuotaAt(cut, stage string) {
	if cut == stage {
		os.Exit(23)
	}
}
