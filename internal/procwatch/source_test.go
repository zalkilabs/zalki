//go:build darwin || linux

package procwatch

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// These tests use the real platform exit source (kqueue on macOS, pidfd and
// inotify on Linux) with real child processes.

func openExitSource(t *testing.T) Source {
	t.Helper()
	source, err := NewSource()
	if err != nil {
		t.Skipf("exit notifications unavailable on this host: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

func startChild(t *testing.T) (*exec.Cmd, Identity) {
	t.Helper()
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	p, err := Read(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return child, Identity{PID: p.PID, Start: p.Start}
}

func nextEvent(t *testing.T, source Source, within time.Duration) (Event, bool) {
	t.Helper()
	select {
	case event := <-source.Events():
		return event, true
	case <-time.After(within):
		return Event{}, false
	}
}

func TestExitSourceReportsAKilledChildPromptly(t *testing.T) {
	source := openExitSource(t)
	child, ref := startChild(t)
	if err := source.WatchProcess(ref); err != nil {
		t.Fatal(err)
	}
	if err := source.WatchProcess(ref); err != nil { // idempotent
		t.Fatal(err)
	}
	if event, ok := nextEvent(t, source, 200*time.Millisecond); ok {
		t.Fatalf("event for a running process: %+v", event)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	event, ok := nextEvent(t, source, 2*time.Second)
	if !ok || event.Kind != ProcessExited || event.Process != ref {
		t.Fatalf("event: %+v %v", event, ok)
	}
	if event, ok := nextEvent(t, source, 200*time.Millisecond); ok {
		t.Fatalf("duplicate event: %+v", event)
	}
}

func TestExitSourceTreatsAnAlreadyExitedProcessAsAnExit(t *testing.T) {
	source := openExitSource(t)
	child, ref := startChild(t)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait() // reaped: the pid no longer exists
	if err := source.WatchProcess(ref); err != nil {
		t.Fatal(err)
	}
	event, ok := nextEvent(t, source, 2*time.Second)
	if !ok || event.Kind != ProcessExited || event.Process != ref {
		t.Fatalf("event: %+v %v", event, ok)
	}
}

func TestExitSourceDoesNotAttachToAReusedPID(t *testing.T) {
	source := openExitSource(t)
	_, ref := startChild(t)
	stale := ref
	stale.Start = "an-earlier-process-at-this-pid"
	if err := source.WatchProcess(stale); err != nil {
		t.Fatal(err)
	}
	event, ok := nextEvent(t, source, 2*time.Second)
	if !ok || event.Kind != ProcessExited || event.Process != stale {
		t.Fatalf("a reused pid was not reported as the original's exit: %+v %v", event, ok)
	}
	// The live process itself can still be watched afterwards.
	if err := source.WatchProcess(ref); err != nil {
		t.Fatal(err)
	}
	if event, ok := nextEvent(t, source, 200*time.Millisecond); ok {
		t.Fatalf("event for the live process: %+v", event)
	}
}

func TestExitSourceUnwatchStopsEvents(t *testing.T) {
	source := openExitSource(t)
	child, ref := startChild(t)
	if err := source.WatchProcess(ref); err != nil {
		t.Fatal(err)
	}
	source.Unwatch(ref)
	source.Unwatch(ref) // unknown ref is a no-op
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if event, ok := nextEvent(t, source, 400*time.Millisecond); ok {
		t.Fatalf("event after Unwatch: %+v", event)
	}
}

func TestExitSourceReportsDirectoryChanges(t *testing.T) {
	source := openExitSource(t)
	dir := filepath.Join(t.TempDir(), "watches")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := source.WatchDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := source.WatchDir(dir); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entry.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if event, ok := nextEvent(t, source, 2*time.Second); !ok || event.Kind != DirectoryChanged {
		t.Fatalf("no event for a new entry: %+v %v", event, ok)
	}
	// Removing and recreating the directory ends the watch; it can be watched again.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if event, ok := nextEvent(t, source, 2*time.Second); !ok || event.Kind != DirectoryChanged {
		t.Fatalf("no event for the removed directory: %+v %v", event, ok)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Drain anything still queued before checking the new watch.
	for {
		if _, ok := nextEvent(t, source, 100*time.Millisecond); !ok {
			break
		}
	}
	if err := source.WatchDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "again.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if event, ok := nextEvent(t, source, 2*time.Second); !ok || event.Kind != DirectoryChanged {
		t.Fatalf("no event after re-watching: %+v %v", event, ok)
	}
}

func TestExitSourceDoesNotFollowADirectorySymlink(t *testing.T) {
	source := openExitSource(t)
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := source.WatchDir(link); err == nil {
		t.Fatal("watched through a symlink")
	}
	if err := source.WatchDir(filepath.Join(target, "missing")); err == nil {
		t.Fatal("watched a missing directory")
	}
}

func TestExitSourceClosesPromptlyAndIsIdempotent(t *testing.T) {
	source, err := NewSource()
	if err != nil {
		t.Skipf("exit notifications unavailable on this host: %v", err)
	}
	_, ref := startChild(t)
	if err := source.WatchProcess(ref); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- source.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
}
