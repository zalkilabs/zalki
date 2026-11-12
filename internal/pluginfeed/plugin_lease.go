package pluginfeed

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/journal"
)

// OpenPluginLease acquires a read-only empty private lease. Close it after child exit.
// Read-only FD3 prevents accidental writes, not same-user reopening or LOCK_UN.
func OpenPluginLease(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := CheckPluginLeaseFD(fd); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := journal.LockExclusive(fd, true); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("plugin host or child lease unavailable for %q: %w; use lsof on that exact file, confirm the plugin/helper, stop it, retry; never delete or truncate a live lease", path, err)
	}
	if err := errors.Join(file.Sync(), journal.SyncDir(filepath.Dir(path))); err != nil {
		_ = file.Close()
		return nil, err
	}
	// Never explicitly unlock: the reference child shares this open description.
	return file, nil
}

// CheckPluginLeaseFD validates a plugin lease descriptor without releasing it.
func CheckPluginLeaseFD(fd int) error {
	var info syscall.Stat_t
	if err := syscall.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Size != 0 || info.Mode&0077 != 0 || info.Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t.
		return errors.New("plugin lease must be an empty private regular file owned by this user")
	}
	return nil
}
