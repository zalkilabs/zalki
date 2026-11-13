package pluginfeed

import (
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

// S07: while the journal lock is held, the supervisor's snapshot still
// reads the current record; PluginStatus, which takes that lock, waits.
func TestPluginSnapshotDoesNotUseStoreLock(t *testing.T) {
	store := fixtureStore(t)
	f := installForTest(t, store)
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = bus.WithJournalWrite(store, func(bus.JournalView) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)
	read := func(fn func() (PluginFeedStatus, error)) (PluginFeedStatus, bool) {
		done := make(chan PluginFeedStatus, 1)
		go func() {
			status, _ := fn()
			done <- status
		}()
		select {
		case status := <-done:
			return status, true
		case <-time.After(500 * time.Millisecond):
			return PluginFeedStatus{}, false
		}
	}
	snapshot, ok := read(f.PluginSnapshot)
	if !ok || snapshot.ID == "" || snapshot.Name != f.name {
		t.Fatalf("the snapshot waited for the journal lock or read nothing: %v %+v", ok, snapshot)
	}
	if _, ok := read(f.PluginStatus); ok {
		t.Fatal("the control did not wait: the journal lock was not held")
	}
}
