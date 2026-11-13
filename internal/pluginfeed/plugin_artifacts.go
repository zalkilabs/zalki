package pluginfeed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// errForeignArtifact refuses to remove a file Zalki cannot prove it made.
func errForeignArtifact(path string) error {
	return model.NewError(model.CodeConflict, "%s is not a file Zalki made for this plugin; nothing was removed. Check it and remove it yourself if it is not needed", path)
}

// checkInstallArtifacts proves, before anything is removed, that every
// leftover artifact of this name is Zalki's own:
//   - the pin folder is a real, private folder of this user (no link) that
//     holds only pins: regular files named by the SHA-256 of their content;
//   - each ledger is a private regular file of this user that decodes
//     strictly, passes the ledger's own validity check, and belongs to current (the installation being removed), or,
//     when current is "", to an installation that no longer exists (no
//     identity file): an orphan of an earlier, removed installation.
//
// Anything else (a link, an unknown file, another live installation's
// ledger) is refused with its path and nothing is removed.
func (s PluginFeed) checkInstallArtifacts(current string) error {
	if err := s.checkPinFolder(); err != nil {
		return err
	}
	var reply replyLedger
	if err := s.checkLedger(s.replyLedgerPath(), &reply, func() (string, bool) { return reply.Installation, reply.valid() }, current); err != nil {
		return err
	}
	var notice startNoticeLedger
	return s.checkLedger(s.startNoticePath(), &notice, func() (string, bool) {
		return notice.Installation, model.IsCanonicalUUID(notice.Installation) && notice.valid(notice.Installation)
	}, current)
}

func ownedByMe(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) //nolint:gosec // G115: getuid returns a Unix uid_t.
}

func (s PluginFeed) checkPinFolder() error {
	dir := s.pluginBinPath()
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByMe(info) {
		return errForeignArtifact(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !validPluginSHA(entry.Name()) || !pinMatches(filepath.Join(dir, entry.Name())) {
			return errForeignArtifact(filepath.Join(dir, entry.Name()))
		}
	}
	return nil
}

// pinMatches reports a regular file of this user, opened without following
// links, whose content hashes to its own name.
func pinMatches(path string) bool {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close() //nolint:errcheck // Read-only check.
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByMe(info) {
		return false
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return false
	}
	return hex.EncodeToString(sum.Sum(nil)) == filepath.Base(path)
}

// checkLedger: shape is the ledger's own validity check (the same the host
// uses when it reads it), so a skeleton or hand-made file is never owned.
func (s PluginFeed) checkLedger(path string, into any, shape func() (string, bool), current string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ownedByMe(info) {
		return errForeignArtifact(path)
	}
	raw, err := journal.ReadBytes(path, 1<<20)
	if err != nil || bus.DecodeStrictRecordJSON(raw, into) != nil {
		return errForeignArtifact(path)
	}
	owner, ok := shape()
	switch {
	case !ok:
		return errForeignArtifact(path)
	case current != "" && owner == current:
		return nil
	case current == "" && model.IsCanonicalUUID(owner) && !s.identityExists(owner):
		return nil // an orphan of a removed installation of this name
	}
	return errForeignArtifact(path)
}

// identityExists reports whether an installation's identity file is present,
// that is, whether that installation may still be live.
func (s PluginFeed) identityExists(installation string) bool {
	_, err := os.Lstat(filepath.Join(s.pluginDir(), "id-"+installation+".json"))
	return !errors.Is(err, os.ErrNotExist)
}
