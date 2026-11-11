package bus

import (
	"os"
	"path/filepath"
	"testing"
)

// journalSnapshot maps every regular file under the state directory to its
// content, so a test can check that an operation changed nothing.
func journalSnapshot(t *testing.T, store Store) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	if err := filepath.WalkDir(store.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(store.Dir, path)
		if err != nil {
			return err
		}
		snapshot[rel] = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
