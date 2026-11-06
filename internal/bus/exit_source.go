package bus

import (
	"sync"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

// exitSource turns operating-system notifications into events for the exit
// observer. The production wrapper converts procwatch's raw identities and
// events; observer tests can supply this domain-facing interface directly.
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
var errExitSourceUnsupported = procwatch.ErrUnsupported

// processKey identifies one process incarnation, so a reused PID is a new key.
type processKey struct {
	PID   int
	Start string
}

func keyOf(ref ProcessRef) processKey { return processKey{ref.PID, ref.Start} }

// processExitSource keeps the observer's domain contract separate from the raw
// OS source. Its forwarding goroutine stops even when no event consumer remains.
type processExitSource struct {
	source procwatch.Source
	events chan exitEvent
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

func newExitSource() (exitSource, error) {
	source, err := procwatch.NewSource()
	if err != nil {
		return nil, err
	}
	return adaptExitSource(source), nil
}

func adaptExitSource(source procwatch.Source) *processExitSource {
	s := &processExitSource{source: source, events: make(chan exitEvent), done: make(chan struct{})}
	s.wg.Add(1)
	go s.forward()
	return s
}

func (s *processExitSource) WatchProcess(ref ProcessRef) error {
	return s.source.WatchProcess(procwatch.Identity{PID: ref.PID, Start: ref.Start})
}

func (s *processExitSource) Unwatch(ref ProcessRef) {
	s.source.Unwatch(procwatch.Identity{PID: ref.PID, Start: ref.Start})
}

func (s *processExitSource) WatchDir(path string) error { return s.source.WatchDir(path) }

func (s *processExitSource) Events() <-chan exitEvent { return s.events }

func (s *processExitSource) forward() {
	defer s.wg.Done()
	events := s.source.Events()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			select {
			case s.events <- domainExitEvent(event):
			case <-s.done:
				return
			}
		case <-s.done:
			return
		}
	}
}

func domainExitEvent(event procwatch.Event) exitEvent {
	var kind exitEventKind
	switch event.Kind {
	case procwatch.ProcessExited:
		kind = exitProcess
	case procwatch.DirectoryChanged:
		kind = exitDirChanged
	case procwatch.SourceFailed:
		kind = exitSourceFailed
	}
	// Raw exit evidence has no role. The observer correlates only PID/start,
	// then re-reads the session and its domain owner under the journal lock.
	return exitEvent{Kind: kind, Process: ProcessRef{PID: event.Process.PID, Start: event.Process.Start}}
}

func (s *processExitSource) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		err = s.source.Close()
		s.wg.Wait()
	})
	return err
}
