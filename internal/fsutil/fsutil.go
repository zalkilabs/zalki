// Package fsutil holds small durable-write and path helpers shared by the journal, the installer and the adapter registry.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// WriteExclusive creates path with mode 0600 and fails if it already exists.
func WriteExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) //nolint:gosec // G304: callers pass tool-owned paths
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// WriteAtomic replaces path with data through a synced temporary file and rename.
func WriteAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".zalki-config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // The temporary path is already gone after a successful rename.
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir flushes directory metadata so a rename or create survives a crash.
func SyncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // G304: callers pass tool-owned directories
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // Sync reports durability errors before descriptor cleanup.
	return dir.Sync()
}

// SafeRelativePath reports whether path is a clean relative path that stays below its root.
func SafeRelativePath(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

// CheckPrivateDir checks that path is a directory, not a link, owned by the
// current user and closed to group and others. It never creates the directory.
func CheckPrivateDir(path string) error {
	return checkPrivateDirOwner(path, os.Getuid())
}

func checkPrivateDirOwner(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || int64(stat.Uid) != int64(uid) {
		return fmt.Errorf("state directory must be a private directory owned by you: %s", path)
	}
	return nil
}
