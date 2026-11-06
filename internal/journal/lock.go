package journal

import (
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
