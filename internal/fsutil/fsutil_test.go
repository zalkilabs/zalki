package fsutil_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

func TestSafeRelativePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"a", true},
		{"a/b", true},
		{".claude/settings.json", true},
		{".claude.json", true},
		{".hidden/x", true},
		{"..x", true},
		{"a..b/c", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../x", false},
		{"../../x", false},
		{"a/..", false},
		{"a/../b", false},
		{"a/./b", false},
		{"./a", false},
		{"a//b", false},
		{"a/", false},
		{"/", false},
		{"/abs", false},
		{"/abs/file.json", false},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			if got := fsutil.SafeRelativePath(test.path); got != test.want {
				t.Fatalf("SafeRelativePath(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}

func TestCheckPrivateDir(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	open := filepath.Join(root, "open")
	file := filepath.Join(root, "file")
	link := filepath.Join(root, "link")
	for _, dir := range []string{private, open} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(open, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.CheckPrivateDir(private); err != nil {
		t.Fatal("private directory refused", err)
	}
	for _, path := range []string{open, file, link} {
		if err := fsutil.CheckPrivateDir(path); err == nil || err.Error() != "state directory must be a private directory owned by you: "+path {
			t.Error("accepted", path, err)
		}
	}
	if err := fsutil.CheckPrivateDirOwner(private, os.Getuid()+1); err == nil {
		t.Error("accepted a directory owned by another uid")
	}
	missing := filepath.Join(root, "missing")
	if err := fsutil.CheckPrivateDir(missing); !errors.Is(err, os.ErrNotExist) {
		t.Error("missing directory", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("check created the directory", err)
	}
}
