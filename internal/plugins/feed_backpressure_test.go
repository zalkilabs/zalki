package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestPlainFeedPipeBackpressureReleasesStoreAndCancels(t *testing.T) {
	store, session := hostStore(t)
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	owner, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	c, cancel, _, _, _ := gracefulChildFixture(t, "no-read")
	if err := c.input.SetWriteDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.input.Write(make([]byte, 1<<20)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("fixture has no actual backpressure", err)
	}
	host := &v2Host{service: &bus.Service{Store: store}, ctx: c.parent, feed: feed, binding: captureFeedFeatures(&owner, []string{capFeed}), child: c, enabled: []string{capFeed}, acks: make(chan string, 1)}
	done := make(chan error, 1)
	go func() { done <- host.tick() }()
	eventually(t, func() bool {
		return pluginGoroutineContains("plugins.(*v2Host).writeFeedPart", "internal/poll.(*FD).Write")
	})
	fd, err := journal.OpenLockFile(filepath.Join(store.Dir, "lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.CloseLockFile(fd)
	if err := journal.LockExclusive(fd, true); err != nil {
		t.Fatal("F_HOST_BLOCKED_PIPE_NEVER_HOLDS_STORE_LOCK", err)
	}
	if err := journal.Unlock(fd); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal("F_HOST_NATIVE_OPERATION_PROGRESS_DURING_BLOCKED_FEED", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errChildCanceled) {
			t.Fatal("F_HOST_BATCH_CANCEL_RETAINS_PIPE_CAUSE", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("F_HOST_BATCH_CANCEL_UNBLOCKS_ACTUAL_PIPE")
	}
	if host.offer != nil || !host.offerAt.IsZero() {
		t.Fatal("F_HOST_ACK_TIMEOUT_STARTS_ONLY_AFTER_COMPLETE_END")
	}
}
