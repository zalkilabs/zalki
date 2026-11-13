package pluginfeed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Tests interleave a foreign entry after the complete proof, before deletion.
var beforeUpdatePinSweep = func() {}

// Delete only captured, proven names from the opened directory. A second
// complete inventory check precedes every deletion; a late addition is never
// enumerated as a new deletion candidate. This is ownership bookkeeping, not
// isolation from another process deliberately changing the same user's files.
func (s PluginFeed) sweepOwnedUpdatePins(current string) (err error) {
	root, identity, err := openUpdatePinRoot(s.pluginBinPath())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	pins, err := captureUpdatePins(root)
	if err != nil {
		return err
	}
	if _, exists := pins[filepath.Base(current)]; !exists {
		return errForeignArtifact(current)
	}
	beforeUpdatePinSweep()
	if err := verifyUpdatePins(root, pins); err != nil {
		return err
	}
	info, err := os.Lstat(s.pluginBinPath())
	if err != nil || !sameUpdatePin(identity, info) {
		return errors.Join(err, errForeignArtifact(s.pluginBinPath()))
	}
	for name, proven := range pins {
		if name == filepath.Base(current) {
			continue
		}
		info, err := root.Lstat(name)
		if err != nil || !sameUpdatePin(proven, info) {
			return errors.Join(err, errForeignArtifact(filepath.Join(root.Name(), name)))
		}
		if err := root.Remove(name); err != nil {
			return err
		}
	}
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func openUpdatePinRoot(path string) (*os.Root, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ownedByMe(info) {
		return nil, nil, errForeignArtifact(path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return nil, nil, errors.Join(err, errForeignArtifact(path), root.Close())
	}
	return root, opened, nil
}

func updatePinEntries(root *os.Root) ([]os.DirEntry, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := file.ReadDir(-1)
	return entries, errors.Join(err, file.Close())
}

func captureUpdatePins(root *os.Root) (map[string]os.FileInfo, error) {
	entries, err := updatePinEntries(root)
	if err != nil {
		return nil, err
	}
	pins := make(map[string]os.FileInfo, len(entries))
	for _, entry := range entries {
		info, err := proveUpdatePin(root, entry.Name())
		if err != nil {
			return nil, err
		}
		pins[entry.Name()] = info
	}
	return pins, nil
}

func proveUpdatePin(root *os.Root, name string) (os.FileInfo, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !validPluginSHA(name) || !info.Mode().IsRegular() || !ownedByMe(info) {
		return nil, errForeignArtifact(filepath.Join(root.Name(), name))
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !sameUpdatePin(info, opened) {
		return nil, errors.Join(statErr, errForeignArtifact(filepath.Join(root.Name(), name)), file.Close())
	}
	sum := sha256.New()
	_, copyErr := io.Copy(sum, io.LimitReader(file, info.Size()+1))
	err = errors.Join(copyErr, file.Close())
	if err != nil {
		return nil, err
	}
	if !sameUpdatePin(info, opened) || hex.EncodeToString(sum.Sum(nil)) != name {
		return nil, errForeignArtifact(filepath.Join(root.Name(), name))
	}
	return info, nil
}

func verifyUpdatePins(root *os.Root, pins map[string]os.FileInfo) error {
	entries, err := updatePinEntries(root)
	if err != nil {
		return err
	}
	if len(entries) != len(pins) {
		return errForeignArtifact(root.Name())
	}
	for _, entry := range entries {
		proven, exists := pins[entry.Name()]
		info, err := root.Lstat(entry.Name())
		if !exists || err != nil || !sameUpdatePin(proven, info) {
			return errors.Join(err, errForeignArtifact(filepath.Join(root.Name(), entry.Name())))
		}
	}
	return nil
}

func sameUpdatePin(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}
