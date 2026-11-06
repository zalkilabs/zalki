package journal_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
)

func TestSyncDir(t *testing.T) {
	for _, test := range []struct {
		name string
		want syscall.Errno
	}{
		{"directory", 0},
		{"missing", syscall.ENOENT},
		{"permission denied", syscall.EACCES},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := dir
			switch test.name {
			case "missing":
				path = filepath.Join(dir, "missing")
			case "permission denied":
				skipIfRoot(t)
				if err := os.Chmod(dir, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(dir, 0700) })
			}
			err := journal.SyncDir(path)
			if test.want == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			pathError, ok := err.(*os.PathError) //nolint:errorlint // Preserve the direct filesystem PathError instead of adding another wrapper.
			if !ok || pathError.Op != "open" || pathError.Path != path {
				t.Fatalf("SyncDir error = %T %v, want direct open PathError for %q", err, err, path)
			}
			requireErrno(t, pathError.Err, test.want)
		})
	}
}
