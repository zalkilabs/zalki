package journal_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"golang.org/x/sys/unix"
)

func requireErrno(t *testing.T, err error, want syscall.Errno) {
	t.Helper()
	got, ok := err.(syscall.Errno) //nolint:errorlint // The primitive must return the direct errno, not a wrapper that merely unwraps to it.
	if !ok || got != want {
		t.Fatalf("error = %T %v, want raw errno %d", err, err, want)
	}
}

func openTestLock(t *testing.T, path string, create bool) int {
	t.Helper()
	fd, err := journal.OpenLockFile(path, create)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func lockPathFixture(t *testing.T, shape string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	switch shape {
	case "existing file", "permission denied":
		writeFile(t, dir, "lock", "preserved")
		permissions := os.FileMode(0640)
		if shape == "permission denied" {
			skipIfRoot(t)
			permissions = 0
		}
		if err := os.Chmod(path, permissions); err != nil {
			t.Fatal(err)
		}
	case "symbolic link":
		target := writeFile(t, dir, "target", "preserved")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	case "directory":
		path = dir
	case "missing parent":
		path = filepath.Join(dir, "missing", "lock")
	}
	return path
}

func checkFailedLockOpen(t *testing.T, shape, path string, fd int, err error, want syscall.Errno) {
	t.Helper()
	if fd != -1 {
		syscall.Close(fd)
		t.Fatalf("failed open returned fd %d", fd)
	}
	requireErrno(t, err, want)
	if shape == "missing file" {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("existing-only open created path: %v", err)
		}
	}
	if shape == "symbolic link" {
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "target")); err != nil || string(data) != "preserved" {
			t.Fatalf("open changed symlink target: %q, %v", data, err)
		}
	}
}

func checkOpenedLock(t *testing.T, shape, path string, fd int) {
	t.Helper()
	wantMode, wantData := os.FileMode(0600), ""
	if shape == "existing file" {
		wantMode, wantData = 0640, "preserved"
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != wantMode {
		t.Fatalf("lock mode = %v, want %v", info.Mode().Perm(), wantMode)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != wantData {
		t.Fatalf("lock content = %q, %v, want %q", data, err, wantData)
	}
	if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil || flags != unix.FD_CLOEXEC {
		t.Fatalf("descriptor flags = %d, %v, want CLOEXEC", flags, err)
	}
	if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0); err != nil || flags&unix.O_ACCMODE != unix.O_RDWR {
		t.Fatalf("open flags = %d, %v, want read-write", flags, err)
	}
}

func TestOpenLockFile(t *testing.T) {
	for _, create := range []bool{false, true} {
		for _, test := range []struct {
			name string
			want syscall.Errno
		}{
			{"missing file", syscall.ENOENT},
			{"existing file", 0},
			{"symbolic link", syscall.ELOOP},
			{"directory", syscall.EISDIR},
			{"missing parent", syscall.ENOENT},
			{"permission denied", syscall.EACCES},
		} {
			mode := "existing only/"
			if create {
				mode = "create/"
			}
			t.Run(mode+test.name, func(t *testing.T) {
				path := lockPathFixture(t, test.name)
				want := test.want
				if test.name == "missing file" && create {
					want = 0
				}
				fd, err := journal.OpenLockFile(path, create)
				if want != 0 {
					checkFailedLockOpen(t, test.name, path, fd, err, want)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer journal.CloseLockFile(fd)
				checkOpenedLock(t, test.name, path, fd)
			})
		}
	}
}

// A missing LOCK_NB must fail within a deadline rather than hang the suite.
func tryContendedLock(t *testing.T, holder, contender int) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- journal.LockExclusive(contender, true) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		// Release with the independent syscall, even if Unlock is regressed.
		syscall.Flock(holder, syscall.LOCK_UN)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("contended lock attempt remained stuck after holder release")
		}
		t.Fatal("nonblocking lock attempt waited for the holder")
		return nil
	}
}

func releaseTestHolder(t *testing.T, holder *int, closeHolder bool) {
	t.Helper()
	if closeHolder {
		if err := journal.CloseLockFile(*holder); err != nil {
			t.Fatal(err)
		}
		*holder = -1
		return
	}
	for range 2 {
		if err := journal.Unlock(*holder); err != nil {
			t.Fatalf("unlock (including repeat): %v", err)
		}
	}
}

func TestLockExclusiveAndRelease(t *testing.T) {
	for _, test := range []struct {
		name        string
		nonblocking bool
		closeHolder bool
	}{
		{"blocking acquisition then unlock", false, false},
		{"nonblocking acquisition then unlock", true, false},
		{"close releases held lock", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lock")
			holder := openTestLock(t, path, true)
			defer func() {
				if holder >= 0 {
					journal.CloseLockFile(holder)
				}
			}()
			contender := openTestLock(t, path, false)
			defer journal.CloseLockFile(contender)
			if err := journal.LockExclusive(holder, test.nonblocking); err != nil {
				t.Fatal(err)
			}
			requireErrno(t, tryContendedLock(t, holder, contender), syscall.EWOULDBLOCK)
			// A raw probe proves the holder really owns an exclusive lock.
			requireErrno(t, syscall.Flock(contender, syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK)
			releaseTestHolder(t, &holder, test.closeHolder)
			if err := syscall.Flock(contender, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("holder did not release lock: %v", err)
			}
		})
	}
}

func TestLockErrorsAreRaw(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	fd, err := journal.OpenLockFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.CloseLockFile(fd); err != nil {
		t.Fatal(err)
	}
	// Check double close immediately, before any operation that opens a file.
	requireErrno(t, journal.CloseLockFile(fd), syscall.EBADF)
	for _, test := range []struct {
		name string
		call func(int) error
	}{
		{"blocking closed fd", func(fd int) error { return journal.LockExclusive(fd, false) }},
		{"nonblocking closed fd", func(fd int) error { return journal.LockExclusive(fd, true) }},
		{"unlock closed fd", journal.Unlock},
		{"close invalid fd", journal.CloseLockFile},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireErrno(t, test.call(-1), syscall.EBADF)
		})
	}

}
