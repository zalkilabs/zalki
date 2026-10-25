package install

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// The lock is home-wide, not state-wide: two installations can target the same
// client settings while using different journal directories. Keep the inode
// after unlocking so concurrent waiters always agree on the lock file.
func lockInstallation(home string) (func(), error) {
	fd, err := syscall.Open(filepath.Join(home, ".zalki-install.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	var stat syscall.Stat_t
	if err = syscall.Fstat(fd, &stat); err == nil && (stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Mode&0077 != 0 || stat.Uid != uint32(os.Getuid())) {
		err = errors.New("installation lock must be a private regular file owned by this OS user")
	}
	if err == nil {
		err = syscall.Flock(fd, syscall.LOCK_EX)
	}
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return func() { syscall.Flock(fd, syscall.LOCK_UN); syscall.Close(fd) }, nil
}
