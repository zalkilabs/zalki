package install

import (
	"errors"
	"os"
	"syscall"

	"github.com/zalkilabs/zalki/internal/platform"
)

// The login item's raw OS calls, kept in this one file.

// fileUID is the owner of a file from its Lstat or Stat result.
func fileUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

// currentUID is this process's user as a file owner value.
func currentUID() uint32 {
	return uint32(os.Getuid()) //nolint:gosec // G115: getuid returns a Unix uid_t.
}

// openNoFollow opens path for reading without following a link at the
// leaf and without blocking on a FIFO.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: fixed login-item paths; links refused.
}

// isLinkLoop reports the error an O_NOFOLLOW open gives for a link.
func isLinkLoop(err error) bool { return errors.Is(err, syscall.ELOOP) }

// openLockFile opens (or creates, 0600) the lock file and accepts only a
// private regular file of this user.
func openLockFile(path string) (int, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return -1, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0o077 != 0 || stat.Uid != currentUID() {
		_ = syscall.Close(fd)
		return -1, errLoginItemLockUnsafe
	}
	return fd, nil
}

// tryLock takes the exclusive lock without waiting; held is false when
// another holder has it.
func tryLock(fd int) (held bool, err error) {
	err = platform.LockExclusive(fd, true)
	if platform.LockBusy(err) {
		return false, nil
	}
	return err == nil, err
}

// closeLock releases the lock by closing its descriptor.
func closeLock(fd int) { _ = syscall.Close(fd) }
