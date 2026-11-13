package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAliasCleanupRefusesReplacedRoot(t *testing.T) {
	f, err := newAliasFixture()
	if err != nil {
		t.Fatal(err)
	}
	moved := f.root + ".owned"
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "a")
	if err := os.WriteFile(sentinel, []byte("outside sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.opened.Close(); _ = f.parent.Close(); _ = os.Remove(f.root); _ = os.RemoveAll(moved) })
	if err := os.Rename(f.root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.root); err != nil {
		t.Fatal(err)
	}
	err = f.close()
	raw, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(raw) != "outside sentinel" {
		t.Fatal("ALIAS_CLEANUP_FOLLOWED_REPLACED_ROOT")
	}
	if _, readErr := os.Lstat(filepath.Join(moved, "a")); readErr != nil {
		t.Fatal("ALIAS_CHANGED_ROOT_WAS_TOUCHED")
	}
	if info, readErr := os.Lstat(f.root); readErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("unknown replacement was removed", readErr)
	}
	if err == nil || !strings.Contains(err.Error(), "fixture_changed") {
		t.Fatal("changed fixture did not fail closed", err)
	}
}

func TestAliasFinalRemovalRefusesForeignLeaf(t *testing.T) {
	for _, kind := range []string{"file", "symlink"} {
		t.Run(kind, func(t *testing.T) { checkAliasFinalRemoval(t, kind) })
	}
}

func checkAliasFinalRemoval(t *testing.T, kind string) {
	t.Helper()
	f, err := newAliasFixture()
	if err != nil {
		t.Fatal(err)
	}
	moved := f.root + ".owned"
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("outside sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := unlinkAliasDirectory
	defer func() {
		unlinkAliasDirectory = previous
		_ = f.opened.Close()
		_ = f.parent.Close()
		_ = os.Remove(f.root)
		_ = os.RemoveAll(moved)
	}()
	unlinkAliasDirectory = func(parent *os.Root, name string) error {
		replaceAliasFixtureLeaf(t, f.root, moved, kind, sentinel)
		return previous(parent, name)
	}
	err = f.close()
	info, inspectErr := os.Lstat(f.root)
	if err == nil || inspectErr != nil || (kind == "symlink" && info.Mode()&os.ModeSymlink == 0) {
		t.Fatal("ALIAS_FINAL_REMOVAL_UNLINKED_FOREIGN_LEAF", err, inspectErr)
	}
	raw, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(raw) != "outside sentinel" {
		t.Fatal("outside sentinel changed")
	}
	if kind == "file" {
		raw, readErr = os.ReadFile(f.root)
		if readErr != nil || string(raw) != "foreign leaf" {
			t.Fatal("foreign file changed")
		}
	}
}

func replaceAliasFixtureLeaf(t *testing.T, root, moved, kind, sentinel string) {
	t.Helper()
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if kind == "file" {
		if err := os.WriteFile(root, []byte("foreign leaf"), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.Symlink(sentinel, root); err != nil {
		t.Fatal(err)
	}
}
