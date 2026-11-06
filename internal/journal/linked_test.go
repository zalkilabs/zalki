package journal_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
)

func changeLinkedPath(t *testing.T, path, shape string) string {
	t.Helper()
	switch shape {
	case "missing", "replacement":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if shape == "replacement" {
			writeFile(t, filepath.Dir(path), "lock", "replacement")
		}
	case "symlink to held inode":
		original := filepath.Join(filepath.Dir(path), "held")
		if err := os.Rename(path, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(original, path); err != nil {
			t.Fatal(err)
		}
	case "invalid path parent":
		path = filepath.Join(path, "child")
	}
	return path
}

func TestLinked(t *testing.T) {
	for _, test := range []struct {
		name string
		want bool
	}{
		{"unchanged", true},
		{"missing", false},
		{"replacement", false},
		{"symlink to held inode", true},
		{"invalid path parent", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lock")
			fd := openTestLock(t, path, true)
			defer journal.CloseLockFile(fd)
			path = changeLinkedPath(t, path, test.name)
			got, err := journal.Linked(fd, path)
			if test.name == "invalid path parent" {
				requireErrno(t, err, syscall.ENOTDIR)
			} else if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Linked = %v, want %v", got, test.want)
			}
		})
	}
}

func TestLinkedInvalidDescriptor(t *testing.T) {
	if linked, err := journal.Linked(-1, filepath.Join(t.TempDir(), "lock")); linked {
		t.Fatal("closed fd reported linked")
	} else {
		requireErrno(t, err, syscall.EBADF)
	}
}
