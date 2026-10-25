// Package fsutil holds small durable-write helpers shared by the journal and installer.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteExclusive creates path with mode 0600 and fails if it already exists.
func WriteExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// WriteAtomic replaces path with data through a synced temporary file and rename.
func WriteAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".zalki-config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir flushes directory metadata so a rename or create survives a crash.
func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
