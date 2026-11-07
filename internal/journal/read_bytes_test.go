package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadBytesPreservesWhitespaceAtExactLimit(t *testing.T) {
	raw := []byte(" \n {\"extension\":  9} \n\n")
	path := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBytes(path, len(raw))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("exact read: %q, %v", got, err)
	}
	if _, err := ReadBytes(path, len(raw)-1); !errors.Is(err, ErrInvalidFile) {
		t.Fatalf("overflow: %v", err)
	}
}

func TestReadBytesRejectsNonregularAndUnsafeLimits(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "empty")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path  string
		limit int
	}{{dir, 100}, {link, 100}, {file, -1}, {file, int(^uint(0) >> 1)}} {
		if _, err := ReadBytes(test.path, test.limit); !errors.Is(err, ErrInvalidFile) {
			t.Fatalf("unsafe read %q: %v", test.path, err)
		}
	}
	if got, err := ReadBytes(file, 0); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty regular file = %v, %v", got, err)
	}
}

func TestReadBytesDoesNotBlockOnFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadBytes(path, 100); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidFile) {
			t.Fatalf("fifo = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read blocked on fifo before checking regular-file status")
	}
}
