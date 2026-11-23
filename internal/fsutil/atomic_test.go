package fsutil_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
}

func TestWriteAtomic(t *testing.T) {
	t.Run("creates and replaces with private permissions and no leftovers", checkWriteAtomicReplaces)
	t.Run("an unwritable directory fails and keeps the old record", checkWriteAtomicUnwritableDir)
	t.Run("a failed rename leaves no temporary file", checkWriteAtomicFailedRename)
	t.Run("a missing directory is an error", checkWriteAtomicMissingDir)
}

func checkWriteAtomicReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.json")
	for _, content := range []string{"first", "second"} {
		if err := fsutil.WriteAtomic(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != content {
			t.Fatalf("content %q, %v", b, err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v, want 0600", info.Mode())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %d entries, %v; want only the record", len(entries), err)
	}
}

func checkWriteAtomicUnwritableDir(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "r.json", "old")
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if err := fsutil.WriteAtomic(path, []byte("new")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v, want os.ErrPermission", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("record is %q after a failed write", b)
	}
}

func checkWriteAtomicFailedRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "r.json")
	if err := os.Mkdir(target, 0700); err != nil { // Renaming a file over a directory fails.
		t.Fatal(err)
	}
	if err := fsutil.WriteAtomic(target, []byte("new")); err == nil {
		t.Fatal("expected an error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("directory holds %d entries after a failed write", len(entries))
	}
}

func checkWriteAtomicMissingDir(t *testing.T) {
	err := fsutil.WriteAtomic(filepath.Join(t.TempDir(), "missing", "r.json"), []byte("x"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want os.ErrNotExist", err)
	}
}
