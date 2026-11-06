package fsutil_test

import (
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
