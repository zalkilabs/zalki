package journal_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
)

func TestPluginLeaseRejectsWritesAndKeepsDuplicateLifetime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.lock")
	lease, err := journal.OpenPluginLease(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if _, err := lease.WriteString("accidental fd3 log"); !errors.Is(err, syscall.EBADF) {
		t.Fatal("plugin lease remained writable", err)
	}
	duplicate, err := syscall.Dup(int(lease.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(duplicate) })
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := journal.OpenPluginLease(path); err == nil {
		_ = other.Close()
		t.Fatal("duplicate no longer retains lease")
	}
	if err := syscall.Close(duplicate); err != nil {
		t.Fatal(err)
	}
	duplicate = -1
	other, err := journal.OpenPluginLease(path)
	if err != nil {
		t.Fatal("read-only lease could not be reacquired", err)
	}
	_ = other.Close()
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Fatal("rejected write poisoned lease", err)
	}
}

// Release a blocked FIFO open before failing so a missing NONBLOCK cannot strand
// this test's goroutine or rely on the package timeout as its oracle.
func TestPluginLeaseRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.lock")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		lease, err := journal.OpenPluginLease(path)
		if lease != nil {
			_ = lease.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted as plugin lease")
		}
	case <-time.After(2 * time.Second):
		writer, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal("release fixture FIFO reader", err)
		}
		defer syscall.Close(writer)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("fixture FIFO reader did not finish")
		}
		t.Fatal("plugin lease blocked opening FIFO")
	}
}
