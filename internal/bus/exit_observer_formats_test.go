package bus

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type observerFormatLog struct {
	slog.Handler
	paused chan struct{}
}

func (h observerFormatLog) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if record.Message == "watch.observer_upgrade_required" {
		select {
		case h.paused <- struct{}{}:
		default:
		}
	}
	return err
}

func waitForObserverFence(t *testing.T, paused <-chan struct{}) {
	t.Helper()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not enter its format fence")
	}
}

func TestObserverFutureMarkerCreatesNothingAndNeverDelivers(t *testing.T) {
	for _, watched := range []bool{false, true} {
		t.Run(map[bool]string{false: "no watch directory", true: "existing ended watch"}[watched], func(t *testing.T) {
			service := newTestService(t)
			store := service.Store
			counter := &noticeCounter{}
			service.Deliver = counter.hook
			if watched {
				exitedWatchedTarget(t, service)
			} else if _, err := store.List(); err != nil {
				t.Fatal(err)
			}
			futureStateMarker(t, store)
			before := journalSnapshot(t, store)
			var log bytes.Buffer
			paused := make(chan struct{}, 2)
			service.Store.Logger = slog.New(observerFormatLog{Handler: slog.NewJSONHandler(&log, nil), paused: paused})
			var opened atomic.Int32
			stop := runObserver(t, service, testSweep, func() (exitSource, error) { opened.Add(1); return newFakeExitSource(), nil })
			waitForObserverFence(t, paused)
			if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
				t.Fatal("observer changed fenced state")
			}
			// Repairing the marker must not wake this old observer; only restart does.
			if err := writeJSON(filepath.Join(store.Dir, stateFormatFile), stateFormats{Schema: 1, Formats: map[string]int{"sessions": leaseSessionSchema}}); err != nil {
				t.Fatal(err)
			}
			repaired := journalSnapshot(t, store)
			select {
			case <-paused:
				t.Fatal("observer logged repeatedly while fenced")
			case <-time.After(3 * testSweep):
			}
			stop()
			if opened.Load() != 0 || counter.count() != 0 {
				t.Fatal("fenced observer started a source or delivered", opened.Load(), counter.count())
			}
			if after := journalSnapshot(t, store); !reflect.DeepEqual(repaired, after) {
				t.Fatal("observer resumed after marker repair")
			}
			if strings.Count(log.String(), "watch.observer_upgrade_required") != 1 {
				t.Fatal("format warning was not logged exactly once", log.String())
			}
			if !watched {
				if _, err := os.Stat(filepath.Join(store.Dir, "watches")); !os.IsNotExist(err) {
					t.Fatal("fenced startup created watches directory", err)
				}
			}
			if _, err := os.Stat(filepath.Join(store.Dir, observerStampName)); !os.IsNotExist(err) {
				t.Fatal("fenced observer wrote sweep stamp", err)
			}
		})
	}
}

func TestObserverFormatFenceAfterStartupStaysIdleUntilShutdown(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	owner := selfOwner(t)
	_, _, target := watchedTarget(t, service, owner)
	var log bytes.Buffer
	paused := make(chan struct{}, 2)
	service.Store.Logger = slog.New(observerFormatLog{Handler: slog.NewJSONHandler(&log, nil), paused: paused})
	source := newFakeExitSource()
	stop := runObserver(t, service, neverSweeps, source.open)
	waitFor(t, "owner registration", func() bool { return source.watchingProcess(*owner) })
	setOwner(t, store, target.ID, &deadOwner)
	futureStateMarker(t, store)
	before := journalSnapshot(t, store)
	source.events <- exitEvent{Kind: exitDirChanged}
	waitForObserverFence(t, paused)
	// The producer must be gone before cancellation, rather than queueing events
	// while the fenced consumer waits for its context to end.
	if !source.isClosed() || source.watchingProcess(*owner) || source.watchingDir(filepath.Join(store.Dir, "watches")) {
		t.Fatal("fence retained the event producer or kernel watches")
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("late format fence consumed or wrote records")
	}
	if err := writeJSON(filepath.Join(store.Dir, stateFormatFile), stateFormats{Schema: 1, Formats: map[string]int{"sessions": leaseSessionSchema}}); err != nil {
		t.Fatal(err)
	}
	repaired := journalSnapshot(t, store)
	source.events <- exitEvent{Kind: exitProcess, Process: *owner}
	select {
	case <-paused:
		t.Fatal("late format fence logged more than once")
	case <-time.After(3 * testSweep):
	}
	stop()
	if counter.count() != 0 {
		t.Fatal("observer delivered after format fence")
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(repaired, after) {
		t.Fatal("observer resumed after format repair")
	}
	if strings.Count(log.String(), "watch.observer_upgrade_required") != 1 {
		t.Fatal("late fence did not log exactly once", log.String())
	}
	if source.closeCount() != 1 {
		t.Fatal("shutdown closed the fenced source again", source.closeCount())
	}
}

func TestObserverStampAndConsumptionRespectFormatFence(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	_, _, target := exitedWatchedTarget(t, service)
	futureStateMarker(t, store)
	before := journalSnapshot(t, store)
	if claimed, err := store.claimObservation(time.Minute, time.Now()); claimed || !IsUpgradeRequired(err) {
		t.Fatal("sweep claim ignored marker", claimed, err)
	}
	if _, watches, err := service.consumeIfExited(target.ID); len(watches) != 0 || !IsUpgradeRequired(err) {
		t.Fatal("under-lock consume ignored marker", watches, err)
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("fenced stamp or consume changed state")
	}
}

func TestPruneKeepsObserverStamp(t *testing.T) {
	store := fixtureStore(t)
	register(t, store, "codex")
	path := filepath.Join(store.Dir, observerStampName)
	if err := os.WriteFile(path, []byte("stamp fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		if _, err := store.Prune(dry, nil); err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(path); err != nil || string(raw) != "stamp fixture" {
			t.Fatal("prune touched observer stamp", err)
		}
	}
}
