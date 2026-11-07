package bus

import (
	"context"
	"errors"
	"github.com/zalkilabs/zalki/internal/procwatch"
	"sync"
	"testing"
	"time"
)

// Events deliberately stays open after Close; completion cannot depend on EOF.
type neverClosingExitSource struct {
	events    chan procwatch.Event
	ready     chan struct{}
	closed    chan struct{}
	readyOnce sync.Once
	mu        sync.Mutex
	closes    int
	closeErr  error
}

var _ procwatch.Source = (*neverClosingExitSource)(nil)

func newNeverClosingExitSource() *neverClosingExitSource {
	return &neverClosingExitSource{events: make(chan procwatch.Event), ready: make(chan struct{}), closed: make(chan struct{})}
}
func (*neverClosingExitSource) WatchProcess(procwatch.Identity) error { return nil }
func (*neverClosingExitSource) Unwatch(procwatch.Identity)            {}
func (*neverClosingExitSource) WatchDir(string) error                 { return nil }
func (s *neverClosingExitSource) Events() <-chan procwatch.Event {
	s.readyOnce.Do(func() { close(s.ready) })
	return s.events
}
func (s *neverClosingExitSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	if s.closes == 1 {
		close(s.closed)
	}
	return s.closeErr
}
func (s *neverClosingExitSource) closeCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.closes }

func startSignalObserver(t *testing.T, source *neverClosingExitSource) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	service := newTestService(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.observeExits(ctx, neverSweeps, func() (procwatch.Source, error) { return source, nil })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("observer did not stop in cleanup")
		}
	})
	select {
	case <-source.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("observer never opened source")
	}
	return cancel, done
}
func requireObserverClosed(t *testing.T, source *neverClosingExitSource, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not complete after cancel")
	}
	select {
	case <-source.closed:
	case <-time.After(time.Second):
		t.Fatal("observer lost source Close ownership")
	}
	if got := source.closeCount(); got != 1 {
		t.Fatalf("raw Close calls=%d, want one", got)
	}
	select {
	case _, ok := <-source.Events():
		if !ok {
			t.Fatal("source event channel was closed")
		}
	default:
	}
}
func TestObserverIdleCancelClosesNeverClosingSource(t *testing.T) {
	source := newNeverClosingExitSource()
	source.closeErr = errors.New("raw close failed")
	cancel, done := startSignalObserver(t, source)
	cancel()
	requireObserverClosed(t, source, done)
}
func TestObserverPendingNotificationCancelClosesNeverClosingSource(t *testing.T) {
	source := newNeverClosingExitSource()
	source.events = make(chan procwatch.Event, 1)
	source.events <- procwatch.Event{Kind: procwatch.DirectoryChanged}
	cancel, done := startSignalObserver(t, source)
	cancel()
	requireObserverClosed(t, source, done)
}
func TestObserverSourceFailureThenCancelClosesNeverClosingSource(t *testing.T) {
	source := newNeverClosingExitSource()
	source.closeErr = errors.New("raw close failed after source failure")
	cancel, done := startSignalObserver(t, source)
	select {
	case source.events <- procwatch.Event{Kind: procwatch.SourceFailed}:
	case <-time.After(3 * time.Second):
		t.Fatal("observer never consumed source failure")
	}
	cancel()
	requireObserverClosed(t, source, done)
}

func TestObserverUnknownRawEventKeepsSourceActive(t *testing.T) {
	source := newNeverClosingExitSource()
	cancel, done := startSignalObserver(t, source)
	select {
	case source.events <- procwatch.Event{Kind: procwatch.EventKind(99)}:
	case <-time.After(time.Second):
		t.Fatal("observer did not receive unknown event")
	}
	select {
	case source.events <- procwatch.Event{Kind: procwatch.DirectoryChanged}:
	case <-time.After(time.Second):
		t.Fatal("unknown event disabled raw notifications")
	}
	cancel()
	requireObserverClosed(t, source, done)
}

func TestObserverRegistersRawIdentityAndKeepsDomainRole(t *testing.T) {
	service := newTestService(t)
	owner := selfOwner(t)
	owner.Role = "backend"
	_, _, target := watchedTarget(t, service, owner)
	source := newFakeExitSource()
	observer := newSyncObserver(service, source)
	observer.sync(t.Context())
	identity := procwatch.Identity{PID: owner.PID, Start: owner.Start}
	if !source.procs[identity] || len(source.procs) != 1 {
		t.Fatalf("raw registrations=%+v, want only %+v", source.procs, identity)
	}
	current, err := service.Store.session(target.ID)
	if err != nil || current.Owner == nil || current.Owner.Role != "backend" {
		t.Fatalf("domain owner role changed: %+v (%v)", current.Owner, err)
	}
}
