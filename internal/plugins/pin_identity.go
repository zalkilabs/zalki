package plugins

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/model"
)

// childPath is the fixed PATH of every plugin child after its home; nothing
// is inherited from the parent's environment.
const childPath = "/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// childEnv is the complete environment of a plugin child: HOME and a fixed
// PATH. No ZALKI_*, capability, loader or other inherited variable is passed.
func childEnv(home string) []string {
	return []string{"HOME=" + home, "PATH=" + home + childPath}
}

// cleanHome accepts only a clean absolute home for the child.
func cleanHome(home string) error {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return model.NewError(model.CodeInvalidInput, "plugin home must be a clean absolute path")
	}
	return nil
}

// fileIdentity is what the pinned executable must still be when it has
// started: the same file (device, inode), size, mode and change time.
type fileIdentity struct {
	dev, ino uint64
	size     int64
	mode     os.FileMode
	changed  int64
}

var errPinChangedAtStart = model.NewError(model.CodeConflict, "the plugin file changed while it started; install it again")

// pinnedIdentity reads the executable's identity without following a link.
// A link, a non-regular or a non-executable file is a changed pin.
func pinnedIdentity(path string) (fileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fileIdentity{}, model.NewError(model.CodeConflict, "plugin launch changed; disable and enable to pin it again")
	}
	return fileIdentity{dev: uint64(stat.Dev), ino: stat.Ino, size: stat.Size, mode: info.Mode(), changed: changedNS(stat)}, nil //nolint:gosec // G115: a device number is never negative.
}

// stillPinned reports whether the started file is the one that was verified.
func stillPinned(path string, pinned fileIdentity) bool {
	now, err := pinnedIdentity(path)
	return err == nil && now == pinned
}
