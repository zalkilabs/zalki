package bus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

type ownershipExitSource struct {
	*fakeExitSource
	ready     chan struct{}
	readyOnce sync.Once
	calls     atomic.Int32
}

func (s *ownershipExitSource) WatchProcess(ref procwatch.Identity) error {
	s.calls.Add(1)
	err := s.fakeExitSource.WatchProcess(ref)
	if err == nil {
		s.readyOnce.Do(func() { close(s.ready) })
	}
	return err
}

func (s *ownershipExitSource) WatchDir(path string) error {
	s.calls.Add(1)
	return s.fakeExitSource.WatchDir(path)
}

func (s *ownershipExitSource) Close() error {
	_ = s.fakeExitSource.Close()
	return errors.New("expected source close failure")
}

type ownershipObserverLog struct {
	slog.Handler
	records chan string
}

func (h ownershipObserverLog) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	switch record.Message {
	case "watch.observer_source_failed", "message.receipt_saved", "watch.observer_upgrade_required":
		select {
		case h.records <- record.Message:
		default:
		}
	}
	return err
}

type ownershipObserverFixture struct {
	service Service
	source  *ownershipExitSource
	owner   *model.ProcessRef
	target  model.Session
	counter *noticeCounter
	records chan string
	stop    func()
}

func newOwnershipObserver(t *testing.T) ownershipObserverFixture {
	t.Helper()
	service := newTestService(t)
	owner := selfOwner(t)
	_, _, target := watchedTarget(t, service, owner)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	records := make(chan string, 8)
	service.Store.Logger = slog.New(ownershipObserverLog{
		Handler: slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}), records: records,
	})
	source := &ownershipExitSource{fakeExitSource: newFakeExitSource(), ready: make(chan struct{})}
	stop := runObserver(t, service, testSweep, func() (procwatch.Source, error) { return source, nil })
	select {
	case <-source.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not register the live owner")
	}
	return ownershipObserverFixture{service: service, source: source, owner: owner, target: target, counter: counter, records: records, stop: stop}
}

func waitOwnershipRecord(t *testing.T, records <-chan string, want string) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case record := <-records:
			if record == want {
				return
			}
		case <-deadline.C:
			t.Fatalf("observer did not record %s", want)
		}
	}
}

func assertFailedSourceRetained(t *testing.T, f ownershipObserverFixture, calls int32) {
	t.Helper()
	if f.source.closeCount() != 0 || !f.source.watchingProcess(*f.owner) || !f.source.watchingDir(filepath.Join(f.service.Store.Dir, "watches")) {
		t.Fatal("source failure closed the owned source before fence or shutdown")
	}
	if f.source.calls.Load() != calls || f.counter.count() != 1 {
		t.Fatal("fallback sweep reused the failed source or lost the notice")
	}
}

func assertFencedSourceClosed(t *testing.T, f ownershipObserverFixture) {
	t.Helper()
	if f.source.closeCount() != 1 || f.source.watchingProcess(*f.owner) || f.source.watchingDir(filepath.Join(f.service.Store.Dir, "watches")) {
		t.Fatal("fence did not release the retained source and registrations exactly once")
	}
}

func assertFencedSourceStayedIdle(t *testing.T, f ownershipObserverFixture, before map[string]string) {
	t.Helper()
	assertFencedSourceClosed(t, f)
	if f.counter.count() != 1 || !reflect.DeepEqual(before, journalSnapshot(t, f.service.Store)) {
		t.Fatal("fenced observer delivered or changed state during shutdown")
	}
	select {
	case event, ok := <-f.source.Events():
		if !ok || event.Kind != procwatch.ProcessExited {
			t.Fatal("source channel closed or lost the post-fence event")
		}
	default:
		t.Fatal("fenced observer consumed a raw event")
	}
	select {
	case <-f.source.Events():
		t.Fatal("source event channel did not stay open and empty")
	default:
	}
}

func TestObserverSourceFailureThenFenceThenCancelKeepsCloseOwnership(t *testing.T) {
	f := newOwnershipObserver(t)
	f.source.events <- procwatch.Event{Kind: procwatch.SourceFailed}
	waitOwnershipRecord(t, f.records, "watch.observer_source_failed")
	calls := f.source.calls.Load()
	setOwner(t, f.service.Store, f.target.ID, &deadOwner)
	waitOwnershipRecord(t, f.records, "message.receipt_saved")
	assertFailedSourceRetained(t, f, calls)
	futureStateMarker(t, f.service.Store)
	waitOwnershipRecord(t, f.records, "watch.observer_upgrade_required")
	assertFencedSourceClosed(t, f)
	before := journalSnapshot(t, f.service.Store)
	f.source.events <- procwatch.Event{Kind: procwatch.ProcessExited}
	f.stop()
	assertFencedSourceStayedIdle(t, f, before)
}
