package procwatch

import (
	"errors"
	"os"
	"syscall"
)

// Source turns operating-system notifications into raw identity events.
// Implementations are safe for concurrent use. Methods never wait on the event
// consumer, which may itself be calling WatchProcess, Unwatch or WatchDir.
type Source interface {
	// WatchProcess reports ref's exit as a ProcessExited event. It is idempotent.
	// An already-gone or reused PID is reported immediately. An error means the
	// process could not be watched; callers may still inspect it periodically.
	WatchProcess(ref Identity) error
	// Unwatch stops watching ref. An unknown identity is a no-op.
	Unwatch(ref Identity)
	// WatchDir reports changes in path, not its subdirectories. It is idempotent
	// and does not follow a symlink. Removal ends the watch.
	WatchDir(path string) error
	// Events delivers events until Close. A failed source reports SourceFailed
	// once and stops producing events. Close does not close the channel.
	Events() <-chan Event
	// Close releases watches and stops the producer. Repeated calls are safe.
	Close() error
}

// EventKind distinguishes process exits, directory changes and source failure.
type EventKind int

const (
	// ProcessExited identifies an exited or replaced process incarnation.
	ProcessExited EventKind = iota + 1
	// DirectoryChanged tells the caller to rescan its watched directory.
	DirectoryChanged
	// SourceFailed reports that notifications stopped; callers may use polling.
	SourceFailed
)

// Event is an operating-system notification without a session identity or role.
type Event struct {
	// Kind describes the notification.
	Kind EventKind
	// Process is set for ProcessExited.
	Process Identity
}

// ErrUnsupported means this platform cannot provide process exit notifications.
var ErrUnsupported = errors.New("process exit notifications are not supported on this platform")

// processGone is Read's evidence that the process no longer exists.
func processGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}
