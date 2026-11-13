package bus

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPromptRegistrationConcurrentProcesses(t *testing.T) {
	if dir := os.Getenv("ZALKI_PROMPT_TEST_STATE"); dir != "" {
		runPromptProcessChild(t, dir)
		return
	}
	assertPromptRegistrationProcesses(t, false)
}

func TestPromptRegistrationConcurrentGlobalQuota(t *testing.T) {
	assertPromptRegistrationProcesses(t, true)
}

func assertPromptRegistrationProcesses(t *testing.T, global bool) {
	t.Helper()
	if global {
		t.Setenv("ZALKI_PROMPT_TEST_GLOBAL", "1")
	}
	store := fixtureStore(t)
	if err := store.locked(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	fd, err := journal.OpenLockFile(filepath.Join(store.Dir, "lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.CloseLockFile(fd)
	if err := journal.LockExclusive(fd, false); err != nil {
		t.Fatal(err)
	}
	native := newID()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	children, logs := startPromptChildren(t, ctx, store.Dir, native)
	defer func() { cancel(); _ = journal.Unlock(fd); waitPromptChildren(children) }()
	waitPromptProcessReady(t, store.Dir, len(children))
	if err := journal.Unlock(fd); err != nil {
		t.Fatal(err)
	}
	for index, child := range children {
		if err := child.Wait(); err != nil {
			t.Fatalf("C05_PROCESS_REGISTRATION: child%d %v %s", index, err, &logs[index])
		}
	}
	assertPromptProcessCounts(t, store, global)
}

func assertPromptProcessCounts(t *testing.T, store Store, global bool) {
	t.Helper()
	expected := uint64(1)
	if global {
		expected = 20
	}
	if meta := eventState(t, store); meta.Count != expected || eventAt(t, store, 1).Type != model.EventSessionRegistered {
		t.Fatal("C05_EXACTLY_ONE_REGISTRATION_EVENT", meta)
	}
	quota, err := readPromptQuota(filepath.Join(store.Dir, "hook-registration-quota.json"))
	if err != nil || len(quota.Attempts) != int(expected) {
		t.Fatal("C05_LOSERS_DO_NOT_CHARGE_OR_ROTATE", err, quota)
	}
}

func runPromptProcessChild(t *testing.T, dir string) {
	t.Helper()
	store := Store{Dir: dir, Registry: testRegistry()}
	if err := os.WriteFile(filepath.Join(dir, "ready-"+os.Getenv("ZALKI_PROMPT_TEST_CHILD")), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("ZALKI_PROMPT_TEST_GLOBAL") == "1" {
		runGlobalPromptChild(t, store)
		return
	}
	native := os.Getenv("ZALKI_PROMPT_TEST_NATIVE")
	if _, err := prepareHookRuntime(store, "cursor", "cursor:"+native, trustedPrompt(native)); err != nil {
		t.Fatal(err)
	}
}

func startPromptChildren(t *testing.T, ctx context.Context, dir, native string) ([]*exec.Cmd, []bytes.Buffer) {
	t.Helper()
	children := make([]*exec.Cmd, 0, 6)
	logs := make([]bytes.Buffer, 6)
	for index := range logs {
		child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPromptRegistrationConcurrentProcesses$", "-test.count=1")
		child.Env = append(os.Environ(), "ZALKI_PROMPT_TEST_STATE="+dir, "ZALKI_PROMPT_TEST_NATIVE="+native, "ZALKI_PROMPT_TEST_CHILD="+strconv.Itoa(index))
		child.Stdout, child.Stderr, child.WaitDelay = &logs[index], &logs[index], time.Second
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
	return children, logs
}

func waitPromptProcessReady(t *testing.T, dir string, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		ready := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ready-") {
				ready++
			}
		}
		if ready == count {
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("C05_CHILDREN_DID_NOT_REACH_BARRIER")
}

func waitPromptChildren(children []*exec.Cmd) {
	for _, child := range children {
		if child.ProcessState == nil {
			_ = child.Wait()
		}
	}
}

func TestPromptRegistrationCrashAfterDurableDebit(t *testing.T) {
	if dir := os.Getenv("ZALKI_PROMPT_CRASH_STATE"); dir != "" {
		crashAfterPromptDebit(t, dir, os.Getenv("ZALKI_PROMPT_CRASH_NATIVE"))
		return
	}
	store := fixtureStore(t)
	native := newID()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPromptRegistrationCrashAfterDurableDebit$", "-test.count=1")
	child.Env = append(os.Environ(), "ZALKI_PROMPT_CRASH_STATE="+store.Dir, "ZALKI_PROMPT_CRASH_NATIVE="+native)
	err := child.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatal("C08_EXPECTED_CRASH_AFTER_DEBIT", err)
	}
	if _, err := store.session("cursor:" + native); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("C08_CRASH_CREATED_NO_SESSION", err)
	}
	if _, err := prepareHookRuntime(store, "cursor", "cursor:"+native, trustedPrompt(native)); !errors.Is(err, errPromptQuota) {
		t.Fatal("C08_RESTART_CANNOT_RESET_DEBIT", err)
	}
}

func crashAfterPromptDebit(t *testing.T, dir, native string) {
	t.Helper()
	store := Store{Dir: dir, Registry: testRegistry()}
	err := store.locked(func() error {
		_, err := registerPromptAbsent(store, "cursor", "cursor:"+native, trustedPrompt(native), time.Now().UTC(), func(path string, raw []byte) error {
			if err := journal.WriteAtomic(path, raw); err != nil {
				return err
			}
			os.Exit(23)
			return nil
		})
		return err
	})
	t.Fatal("C08_CRASH_POINT_NOT_REACHED", err)
}

func runGlobalPromptChild(t *testing.T, store Store) {
	t.Helper()
	index, err := strconv.Atoi(os.Getenv("ZALKI_PROMPT_TEST_CHILD"))
	if err != nil {
		t.Fatal(err)
	}
	for offset := range 4 {
		actor := promptQuotaActor(index*4 + offset)
		native := strings.TrimPrefix(actor, "cursor:")
		if _, err := prepareHookRuntime(store, "cursor", actor, trustedPrompt(native)); err != nil && !errors.Is(err, errPromptQuota) {
			t.Fatal("C07_GLOBAL_PROCESS_REGISTRATION", err)
		}
	}
}
