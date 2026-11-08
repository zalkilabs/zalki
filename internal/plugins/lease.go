package plugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/journal"
)

func hostLease(path string) (*os.File, error) {
	fd, err := journal.OpenLockFile(path, true)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), path)
	if err := checkPrivateFD(fd); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := journal.LockExclusive(fd, true); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("plugin host or child lease unavailable: %w", err)
	}
	if err := errors.Join(file.Sync(), journal.SyncDir(filepath.Dir(path))); err != nil {
		_ = file.Close()
		return nil, err
	}
	// Never explicitly unlock: the reference child shares this open description.
	return file, nil
}

func checkPrivateFD(fd int) error {
	var info syscall.Stat_t
	if err := syscall.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Size != 0 || info.Mode&0077 != 0 || info.Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t.
		return errors.New("plugin lease must be an empty private regular file owned by this user")
	}
	return nil
}
