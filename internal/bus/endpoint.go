package bus

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/model"
)

func InspectEndpoint(path string) (*model.Endpoint, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("expected absolute messaging socket path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	dir, err := os.Stat(parent)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 || dir.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return nil, errors.New("messaging socket directory must be private to this OS user")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return nil, errors.New("expected same-user socket, not a file or symlink")
	}
	stat := info.Sys().(*syscall.Stat_t)
	return &model.Endpoint{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino, ChangedNS: changedNS(stat)}, nil // #nosec G115 -- Preserve opaque device identity; Darwin's dev_t is signed, Linux's is unsigned.
}

func CheckEndpoint(expected *model.Endpoint) error {
	if expected == nil {
		return errors.New("no bound messaging socket")
	}
	current, err := InspectEndpoint(expected.Path)
	if err != nil {
		return err
	}
	if *current != *expected {
		return errors.New("messaging socket was replaced; its listener must register again")
	}
	return nil
}
