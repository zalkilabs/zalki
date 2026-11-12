package bus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"

	"golang.org/x/sys/unix"
)

// Raw probes are independent of the bus implementation being characterized.
func assertRawLockState(t *testing.T, path string, held bool) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if held && !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("independent probe of %s = %v, want held", path, err)
	}
	if !held && err != nil {
		t.Fatalf("independent probe of %s = %v, want released", path, err)
	}
}

type journalLockChild struct {
	input io.WriteCloser
	lines <-chan string
	done  <-chan error
}

func startJournalLockChild(t *testing.T, mode, path string, fd int) journalLockChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJournalLockProcessBaseline$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ZALKI_LOCK_BASELINE_MODE="+mode, "ZALKI_LOCK_BASELINE_PATH="+path, "ZALKI_LOCK_BASELINE_FD="+strconv.Itoa(fd))
	cmd.WaitDelay = time.Second
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		defer close(lines)
		defer close(done)
		defer func() { done <- cmd.Wait() }()
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- "stdout scan failed: " + err.Error():
			case <-ctx.Done():
			}
		}
	}()
	t.Cleanup(func() {
		input.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("lock helper did not exit after cancellation")
		}
	})
	return journalLockChild{input: input, lines: lines, done: done}
}

func expectLockChildLine(t *testing.T, child journalLockChild, want string) {
	t.Helper()
	select {
	case got, ok := <-child.lines:
		if !ok || got != want {
			t.Fatalf("lock helper phase: got %q (open=%v), want %q", got, ok, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("lock helper timed out waiting for %q: possible blocking or leaked lock", want)
	}
}

func releaseLockChild(t *testing.T, child journalLockChild) {
	t.Helper()
	if _, err := io.WriteString(child.input, "release\n"); err != nil {
		t.Fatal(err)
	}
}

func waitLockChild(t *testing.T, child journalLockChild) {
	t.Helper()
	select {
	case err := <-child.done:
		if err != nil {
			t.Fatalf("lock helper failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lock helper failed to exit after release")
	}
}

func awaitParentRelease(t *testing.T) {
	t.Helper()
	// The parent command context bounds this read and kills a stuck helper.
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || line != "release\n" {
		t.Fatalf("parent release command = %q, %v", line, err)
	}
}

func TestJournalLockProcessBaseline(t *testing.T) {
	if mode := os.Getenv("ZALKI_LOCK_BASELINE_MODE"); mode != "" {
		journalLockProcessHelper(t, mode)
		return
	}

	t.Run("journal callbacks hold one exclusive lock across processes", func(t *testing.T) {
		store := fixtureStore(t)
		var child journalLockChild
		if err := store.locked(func() error {
			assertRawLockState(t, filepath.Join(store.Dir, "lock"), true)
			child = startJournalLockChild(t, "journal", store.Dir, -1)
			expectLockChildLine(t, child, "ready")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		expectLockChildLine(t, child, "acquired")
		assertRawLockState(t, filepath.Join(store.Dir, "lock"), true)
		releaseLockChild(t, child)
		expectLockChildLine(t, child, "released")
		waitLockChild(t, child)
		assertRawLockState(t, filepath.Join(store.Dir, "lock"), false)
	})

	t.Run("lifetime descriptor closes on exec and releases after parent close", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "listener.lock")
		fd, err := acquireListenerLock(path, journal.OpenLockFile)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if fd >= 0 {
				syscall.Close(fd)
			}
		})
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil || flags != unix.FD_CLOEXEC {
			t.Fatalf("raw listener descriptor flags = %d, %v; want CLOEXEC", flags, err)
		}
		// No ExtraFiles: an unrelated child must not retain this lifetime lock.
		child := startJournalLockChild(t, "lifetime", path, fd)
		expectLockChildLine(t, child, "ready")
		if err := syscall.Close(fd); err != nil {
			t.Fatal(err)
		}
		fd = -1
		assertRawLockState(t, path, false)
		releaseLockChild(t, child)
		expectLockChildLine(t, child, "released")
		waitLockChild(t, child)
		assertRawLockState(t, path, false)
	})
}

func journalLockProcessHelper(t *testing.T, mode string) {
	t.Helper()
	path := os.Getenv("ZALKI_LOCK_BASELINE_PATH")
	switch mode {
	case "journal":
		assertRawLockState(t, filepath.Join(path, "lock"), true)
		fmt.Println("ready")
		store := Store{Dir: path}
		if err := store.locked(func() error {
			assertRawLockState(t, filepath.Join(path, "lock"), true)
			fmt.Println("acquired")
			awaitParentRelease(t)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	case "lifetime":
		fd, err := strconv.Atoi(os.Getenv("ZALKI_LOCK_BASELINE_FD"))
		if err != nil {
			t.Fatal(err)
		}
		linked, err := lockFileStillLinked(fd, path)
		if linked || err != nil && !errors.Is(err, syscall.EBADF) {
			t.Fatalf("exec child retained the lifetime descriptor or inspection failed: linked=%v, %v", linked, err)
		}
		fmt.Println("ready")
		awaitParentRelease(t)
	default:
		t.Fatalf("unknown lock helper mode %q", mode)
	}
	fmt.Println("released")
}
