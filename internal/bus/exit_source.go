package bus

import (
	"errors"
	"os"
	"syscall"
)

// exitSource turns operating-system notifications into events for the exit
// observer. It is handle-based and portable: only the per-platform files
// (exit_source_darwin.go, exit_source_linux.go, exit_source_other.go) touch file
// descriptors, kqueue, inotify or other OS types, so another platform needs one
// new file implementing this interface and newExitSource.
//
// Implementations must be safe for concurrent use. None of the methods may block
// on the event consumer: the observer calls them from the goroutine that reads
// Events.
type exitSource interface {
	// WatchProcess reports ref's exit as an exitProcess event. It is idempotent.
	// If the process is already gone, or the PID now belongs to a process with a
	// different start time, ref's process has exited: the event is sent at once.
	// An error means exits cannot be observed for ref; the sweep still covers it.
	WatchProcess(ref ProcessRef) error
	// Unwatch stops watching ref. It is a no-op for an unknown ref.
	Unwatch(ref ProcessRef)
	// WatchDir reports entries added to, removed from or renamed in the directory
	// at path (not its subdirectories) as exitDirChanged events. It is idempotent
	// and the watch ends by itself when the directory is removed. A symlink is
	// not followed.
	WatchDir(path string) error
	// Events delivers events until Close. A source that fails delivers
	// exitSourceFailed once and nothing after it.
	Events() <-chan exitEvent
	Close() error
}

type exitEventKind int

const (
	exitProcess exitEventKind = iota + 1
	exitDirChanged
	exitSourceFailed
)

type exitEvent struct {
	Kind    exitEventKind
	Process ProcessRef // set for exitProcess
}

// errExitSourceUnsupported means the platform has no implementation; the
// observer then relies on its periodic sweep alone.
var errExitSourceUnsupported = errors.New("process exit notifications are not supported on this platform")

// processKey identifies one process incarnation, so a reused PID is a new key.
type processKey struct {
	PID   int
	Start string
}

func keyOf(ref ProcessRef) processKey { return processKey{ref.PID, ref.Start} }

// processGone is readProcess's evidence that the process no longer exists.
func processGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}
