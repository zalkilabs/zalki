package journal

import (
	"errors"
	"os"
	"syscall"
)

// OpenLockFile opens path read-write without following a symbolic link. When
// create is true it creates a missing file with mode 0600; otherwise it opens
// only an existing file. It returns the raw descriptor and syscall error,
// without adding close-on-exec. The caller owns locking and descriptor cleanup.
func OpenLockFile(path string, create bool) (int, error) {
	flags := syscall.O_RDWR | syscall.O_NOFOLLOW
	var mode uint32
	if create {
		flags |= syscall.O_CREAT
		mode = 0600
	}
	return syscall.Open(path, flags, mode)
}

// LockExclusive takes an exclusive flock on fd. If nonblocking is true it adds
// LOCK_NB. It makes one syscall and returns its error unchanged, including
// contention or interruption; the caller decides whether to retry.
func LockExclusive(fd int, nonblocking bool) error {
	flags := syscall.LOCK_EX
	if nonblocking {
		flags |= syscall.LOCK_NB
	}
	return syscall.Flock(fd, flags)
}

// Unlock releases fd's flock with one syscall and returns its raw error.
// It does not close the descriptor.
func Unlock(fd int) error {
	return syscall.Flock(fd, syscall.LOCK_UN)
}

// CloseLockFile closes fd with one syscall and returns its raw error.
// It does not explicitly unlock first or hide an already-closed descriptor.
func CloseLockFile(fd int) error {
	return syscall.Close(fd)
}

// Linked reports whether path names the device and inode referenced by fd.
// A missing path reports false without error; other syscall errors pass through
// unchanged. It follows path's final symbolic link, matching syscall.Stat.
// The caller owns synchronization and decides whether to retry a changed path.
func Linked(fd int, path string) (bool, error) {
	var opened syscall.Stat_t
	if err := syscall.Fstat(fd, &opened); err != nil {
		return false, err
	}
	var current syscall.Stat_t
	if err := syscall.Stat(path, &current); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return opened.Dev == current.Dev && opened.Ino == current.Ino, nil
}
