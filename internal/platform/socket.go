//go:build darwin || linux

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// InspectSocket returns the identity of a same-user socket at an absolute path
// whose parent directory is private to this OS user.
func InspectSocket(path string) (SocketID, error) {
	if !filepath.IsAbs(path) {
		return SocketID{}, errors.New("expected absolute messaging socket path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return SocketID{}, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	dir, err := os.Stat(parent)
	if err != nil {
		return SocketID{}, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 || dir.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return SocketID{}, errors.New("messaging socket directory must be private to this OS user")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return SocketID{}, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return SocketID{}, errors.New("expected same-user socket, not a file or symlink")
	}
	stat := info.Sys().(*syscall.Stat_t)
	return SocketID{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino, ChangedNS: ChangedNS(stat)}, nil // #nosec G115 -- Preserve opaque device identity; Darwin's dev_t is signed, Linux's is unsigned.
}
