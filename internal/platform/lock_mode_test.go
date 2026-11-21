//go:build darwin || linux

package platform_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/platform"
)

func TestLockSharedAllowsSharedHoldersAndExcludesExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first := openTestLock(t, path, true)
	second := openTestLock(t, path, false)
	third := openTestLock(t, path, false)
	t.Cleanup(func() {
		_ = platform.CloseLockFile(first)
		_ = platform.CloseLockFile(second)
		_ = platform.CloseLockFile(third)
	})
	if err := platform.LockShared(first, true); err != nil {
		t.Fatal(err)
	}
	if err := platform.LockShared(second, true); err != nil {
		t.Fatalf("a second shared holder was refused: %v", err)
	}
	err := platform.LockExclusive(third, true)
	requireErrno(t, err, syscall.EWOULDBLOCK)
	if !platform.LockBusy(err) {
		t.Fatal("a refused nonblocking exclusive lock was not reported as busy")
	}
	if err := platform.Unlock(first); err != nil {
		t.Fatal(err)
	}
	if err := platform.Unlock(second); err != nil {
		t.Fatal(err)
	}
	if err := platform.LockExclusive(third, true); err != nil {
		t.Fatalf("the exclusive lock stayed refused after both shared holders left: %v", err)
	}
	requireErrno(t, platform.LockShared(first, true), syscall.EWOULDBLOCK)
}

func TestLockBusyNamesOnlyContention(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"contention", syscall.EWOULDBLOCK, true},
		{"wrapped contention", fmt.Errorf("lock: %w", syscall.EWOULDBLOCK), true},
		{"interrupted", syscall.EINTR, false},
		{"bad descriptor", syscall.EBADF, false},
		{"other error", errors.New("other"), false},
	} {
		if got := platform.LockBusy(test.err); got != test.want {
			t.Errorf("%s: LockBusy = %v, want %v", test.name, got, test.want)
		}
	}
}
