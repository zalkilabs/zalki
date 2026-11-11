package bus

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestJournalViewRejectsZeroAndRetainedViews(t *testing.T) {
	assertInactiveJournalView(t, JournalView{})
	store := fixtureStore(t)
	var retained JournalView
	err := WithJournalRead(store, func(view JournalView) error {
		retained = view
		assertJournalLock(t, store, true)
		window, err := view.EventWindow()
		if err != nil || window.Epoch != "" || window.Count != 0 {
			t.Fatal("empty event stream changed", window, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertInactiveJournalView(t, retained)
	assertJournalLock(t, store, false)
}

func assertInactiveJournalView(t *testing.T, view JournalView) {
	t.Helper()
	if _, err := view.EventWindow(); !errors.Is(err, errJournalViewInactive) {
		t.Fatal("inactive view must fail without accessing state", err)
	}
	if _, err := view.EventAt(byteFixtureGen, 1); !errors.Is(err, errJournalViewInactive) {
		t.Fatal("inactive event read must fail", err)
	}
	if _, _, err := view.CurrentMessage(byteFixtureInbox); !errors.Is(err, errJournalViewInactive) {
		t.Fatal("inactive message read must fail", err)
	}
	if err := view.CheckSessionKind(byteFixtureActor); !errors.Is(err, errJournalViewInactive) {
		t.Fatal("inactive session check must fail", err)
	}
	if message, outcome := view.NativeInboxMessage(byteFixtureActor, byteFixtureNative); !reflect.DeepEqual(message, model.Message{}) || outcome != "unavailable" {
		t.Fatal("inactive native projection must be empty and unavailable", outcome)
	}
	if err := view.WithWriteScope("", func() error {
		t.Error("inactive view permitted a write callback")
		return nil
	}); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("inactive writes must return a typed scope error", err)
	}
}

func assertJournalLock(t *testing.T, store Store, held bool) {
	t.Helper()
	fd, err := journal.OpenLockFile(filepath.Join(store.Dir, "lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.CloseLockFile(fd)
	err = journal.LockExclusive(fd, true)
	if (held && !errors.Is(err, syscall.EWOULDBLOCK)) || (!held && err != nil) {
		t.Fatalf("journal lock held=%v: probe returned %v", held, err)
	}
}

func TestJournalViewReleasesOnErrorAndPanic(t *testing.T) {
	store := fixtureStore(t)
	var retained JournalView
	stop := errors.New("callback stopped")
	if err := WithJournalWrite(store, func(view JournalView) error {
		retained = view
		return view.WithWriteScope(store.Dir, func() error {
			if _, err := view.EventWindow(); err != nil {
				t.Fatal("guarded writes must permit nested view reads", err)
			}
			return stop
		})
	}); !errors.Is(err, stop) {
		t.Fatal("callback error was not returned", err)
	}
	assertInactiveJournalView(t, retained)
	assertJournalLock(t, store, false)
	const panicValue = "callback panicked"
	func() {
		defer func() {
			if got := recover(); got != panicValue {
				t.Fatalf("callback panic did not propagate: %v", got)
			}
		}()
		_ = WithJournalWrite(store, func(view JournalView) error {
			retained = view
			return view.WithWriteScope(store.Dir, func() error { panic(panicValue) })
		})
	}()
	assertInactiveJournalView(t, retained)
	assertJournalLock(t, store, false)
}

func TestJournalViewWaitsForBorrowedWriteBeforeUnlock(t *testing.T) {
	store := fixtureStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	operationDone, scopeDone := make(chan error, 1), make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var retained JournalView
	go func() {
		scopeDone <- WithJournalWrite(store, func(view JournalView) error {
			retained = view
			go func() {
				operationDone <- view.WithWriteScope(store.Dir, func() error {
					close(entered)
					<-release
					return nil
				})
			}()
			<-entered
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("guarded operation did not start")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := retained.EventWindow(); errors.Is(err, errJournalViewInactive) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scope did not reject new operations after its callback returned")
		}
		time.Sleep(time.Millisecond)
	}
	assertJournalLock(t, store, true)
	select {
	case err := <-scopeDone:
		t.Fatal("scope unlocked with an active guarded write", err)
	case <-time.After(25 * time.Millisecond):
	}
	unblock()
	if err := <-operationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-scopeDone; err != nil {
		t.Fatal(err)
	}
	assertJournalLock(t, store, false)
}

func TestJournalViewRecoversEventsAndRepliesBeforeCallback(t *testing.T) {
	store, target, _ := stagedLiveCommit(t)
	seedLegacyHeldReply(t, store, target)
	if err := WithJournalRead(store, func(view JournalView) error {
		window, err := view.EventWindow()
		if err != nil || window.Count != 2 || window.Last != 2 {
			t.Fatal("event recovery did not precede callback", window, err)
		}
		if event, err := view.EventAt(window.Epoch, window.Last); err != nil || event.Session.State != model.StateBusy {
			t.Fatal("missing recovered event", event, err)
		}
		current, err := store.session(target.ID)
		if err != nil || current.State != model.StateBusy || current.ApprovalHeld != 1 {
			t.Fatal("reply recovery did not follow event recovery", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
