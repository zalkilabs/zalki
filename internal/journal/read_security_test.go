package journal

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadJSONUsesBoundedRegularReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record")
	raw := []byte(" {\"value\":1,\"extension\":true} \n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var target struct {
		Value int `json:"value"`
	}
	if err := ReadJSON(path, len(raw), &target); err != nil || target.Value != 1 {
		t.Fatal("valid JSON/extension acceptance changed", err)
	}
	if err := ReadJSON(path, len(raw)-1, &target); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("oversize accepted", err)
	}
	for _, limit := range []int{-1, int(^uint(0) >> 1)} {
		if err := ReadJSON(path, limit, &target); !errors.Is(err, ErrInvalidFile) {
			t.Fatal("unsafe limit accepted", err)
		}
	}
	checkUnsafeReadPaths(t, dir, path, &target)
	if err := ReadJSON(filepath.Join(dir, "absent"), 100, &target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing-file class changed", err)
	}
}

func TestReadBytesOpenedInodeAndGrowth(t *testing.T) {
	for _, mode := range []string{"replace", "grow"} {
		t.Run(mode, func(t *testing.T) { checkOpenedRead(t, mode) })
	}
}

func checkOpenedRead(t *testing.T, mode string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "record")
	initial := []byte(`{"v":1}`)
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := readBytes(path, len(initial), func(file *os.File) { changeOpenedRead(t, mode, path, file) })
	if mode == "replace" {
		if err != nil || string(raw) != string(initial) {
			t.Fatalf("read reopened replaced path: %q %v", raw, err)
		}
	} else if !errors.Is(err, ErrInvalidFile) {
		t.Fatal("post-stat valid JSON growth escaped bound", err)
	}
}

func changeOpenedRead(t *testing.T, mode, path string, file *os.File) {
	t.Helper()
	if mode == "replace" {
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"v":2}`), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	writer, err := os.OpenFile(file.Name(), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(" "); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

func checkUnsafeReadPaths(t *testing.T, dir, path string, target any) {
	t.Helper()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(link, 100, target); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("symlink accepted", err)
	}
	if err := ReadJSON(dir, 100, target); !errors.Is(err, ErrInvalidFile) {
		t.Fatal("directory accepted", err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ReadJSON(fifo, 100, target) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidFile) {
			t.Fatal("FIFO accepted", err)
		}
	case <-time.After(time.Second):
		// Release a NONBLOCK mutant so its intended assertion fails, without hanging the package.
		writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = writer.Close()
		}
		<-done
		t.Fatal("JSON reader blocked opening FIFO")
	}
}
