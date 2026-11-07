package journal

import (
	"os"
	"path/filepath"
)

// writeBatchTarget orders the temp data before rename. Its caller must order
// all affected parent directories before deleting the already durable intent.
func writeBatchTarget(path string, data []byte) error {
	return writeBatchTargetWith(path, data, orderBatchFile)
}

func writeBatchTargetWith(path string, data []byte, order func(*os.File) error) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".zalki-redo-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name()) //nolint:errcheck // The rename consumes this name on success.
	if _, err = file.Write(data); err == nil {
		err = order(file)
	}
	closed := file.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	return os.Rename(file.Name(), path)
}

func orderBatchDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // G304: caller validated this batch target parent.
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // Read-only directory descriptor.
	return orderBatchFile(dir)
}

// OrderDirectory submits directory changes before subsequent device writes.
// It is used only inside an intent protocol that retains full durability at
// publication and cleanup; it is not a standalone durable-commit operation.
func OrderDirectory(path string) error { return orderBatchDirectory(path) }
