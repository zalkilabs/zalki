package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// withRestartLock serializes the issuers of a restart request. These tests
// pin what it does with the lock file and with another holder.

func restartLockState(t *testing.T) (state, lockPath string) {
	t.Helper()
	state = t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	return state, filepath.Join(state, "plugins", "demo.restart.lock")
}

func holdRestartLock(t *testing.T, path string, how int) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestRestartLockRunsTheCallbackAndReleasesAfterIt(t *testing.T) {
	state, path := restartLockState(t)
	calls := 0
	boom := errors.New("callback failed")
	err := withRestartLock(state, "demo", func() error {
		calls++
		// Another holder cannot take the lock while the callback runs.
		other, openErr := os.OpenFile(path, os.O_RDWR, 0)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer other.Close()
		if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("lock taken during the callback: %v", err)
		}
		return boom
	})
	if calls != 1 || !errors.Is(err, boom) {
		t.Fatalf("callbacks %d, error %v; want one call and its own error", calls, err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v, %v; want a 0600 file", info, statErr)
	}
	holdRestartLock(t, path, syscall.LOCK_EX) // free again after the call
}

func TestRestartLockBusyGivesUpWithinTheBound(t *testing.T) {
	state, path := restartLockState(t)
	holdRestartLock(t, path, syscall.LOCK_EX)
	called := false
	start := time.Now()
	err := withRestartLock(state, "demo", func() error { called = true; return nil })
	elapsed := time.Since(start)
	if called || model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatalf("called %v, error %v; want an unavailable error and no call", called, err)
	}
	if elapsed < time.Second || elapsed > 4*time.Second {
		t.Fatalf("gave up after %s, want about two seconds", elapsed)
	}
}

func TestRestartLockWaitsForAShortHolder(t *testing.T) {
	state, path := restartLockState(t)
	holder := holdRestartLock(t, path, syscall.LOCK_EX)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = holder.Close()
	}()
	called := false
	if err := withRestartLock(state, "demo", func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("called %v, error %v; want the call after the holder left", called, err)
	}
}

func checkRestartLockRefused(t *testing.T, state string) {
	t.Helper()
	called := false
	err := withRestartLock(state, "demo", func() error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("called %v, error %v; want a refusal and no call", called, err)
	}
}

func TestRestartLockRefusesASymbolicLink(t *testing.T) {
	state, path := restartLockState(t)
	target := filepath.Join(state, "target")
	if err := os.WriteFile(target, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	checkRestartLockRefused(t, state)
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "kept" {
		t.Fatalf("symbolic link target changed: %q, %v", raw, err)
	}
}

func TestRestartLockRefusesAGroupReadableFile(t *testing.T) {
	state, path := restartLockState(t)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	checkRestartLockRefused(t, state)
}

func TestSupervisorProbeSeesOnlyAnExclusiveHolder(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "plugins", ".supervisor.lock")
	if supervisorRunning(state) {
		t.Fatal("a missing lock file was reported as a running controller")
	}
	holder := holdRestartLock(t, path, syscall.LOCK_SH)
	if supervisorRunning(state) {
		t.Fatal("a shared holder was reported as a running controller")
	}
	_ = holder.Close()
	exclusive := holdRestartLock(t, path, syscall.LOCK_EX)
	if !supervisorRunning(state) {
		t.Fatal("an exclusive holder was not reported as a running controller")
	}
	_ = exclusive.Close()
	if supervisorRunning(state) {
		t.Fatal("a released lock was reported as a running controller")
	}
}
