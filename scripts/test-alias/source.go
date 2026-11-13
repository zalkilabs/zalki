package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Fingerprint input paths, modes, content and symlink spellings. Only top-level
// VCS/build outputs/local agent state are excluded; reports live outside repo.
func sourceDigest(root string) (string, int, error) {
	digest := sha256.New()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		included, err := hashSourceInput(root, path, entry, digest)
		if included {
			count++
		}
		return err
	})
	return hex.EncodeToString(digest.Sum(nil)), count, err
}

func hashSourceInput(root, path string, entry fs.DirEntry, digest hash.Hash) (bool, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false, err
	}
	if excludedSourceDirectory(rel, entry) {
		return false, filepath.SkipDir
	}
	if entry.IsDir() {
		return false, nil
	}
	info, err := entry.Info()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return false, fmt.Errorf("source fingerprint refuses non-regular input %q", rel)
	}
	raw, err := readSourceInput(path, info.Mode())
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(raw)
	_, err = fmt.Fprintf(digest, "%s %s %x\n", strconv.Quote(filepath.ToSlash(rel)), info.Mode(), sum)
	return err == nil, err
}

func excludedSourceDirectory(rel string, entry fs.DirEntry) bool {
	if rel == "." || !entry.IsDir() {
		return false
	}
	top, _, _ := strings.Cut(rel, string(filepath.Separator))
	switch top {
	case ".git", "bin", "dist", ".codex", ".agents", ".aws":
		return true
	}
	return false
}

func readSourceInput(path string, mode fs.FileMode) ([]byte, error) {
	if mode&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		return []byte("symlink\x00" + target), err
	}
	return os.ReadFile(path) //nolint:gosec // G304: enumerated regular source input under the operator-selected module root.
}

func verifyAliasSource(repo, expected string, count int) error {
	current, currentCount, err := sourceDigest(repo)
	if err != nil {
		return fmt.Errorf("source_changed: %w", err)
	}
	if current != expected || currentCount != count {
		return errors.New("source_changed: input fingerprint differs after the run")
	}
	return nil
}
