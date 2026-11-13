package plugins

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// ownerChangesBetweenTicks is what an owner can do between two pump ticks of
// a running child: disable, set new feed sessions (if any), enable again.
func ownerChangesBetweenTicks(t *testing.T, feed pluginfeed.PluginFeed, sessions ...string) {
	t.Helper()
	if err := feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if len(sessions) > 0 {
		if err := feed.ConfigurePluginFeed(sessions); err != nil {
			t.Fatal(err)
		}
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
}

// pipeChild returns a child whose frames the test reads from the returned reader.
func pipeChild(t *testing.T) (*child, *bufio.Reader) {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close(); _ = wr.Close() })
	return &child{input: wr}, bufio.NewReader(rd)
}

func firstFrame(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestV2HostOldChildGetsNoNewFeedGrant(t *testing.T) {
	for _, change := range []string{"new sessions", "disable and enable"} {
		t.Run(change, func(t *testing.T) { checkV2OldChild(t, change == "new sessions") })
	}
}

func checkV2OldChild(t *testing.T, newSessions bool) {
	t.Helper()
	x := v2HostFixture(t)
	var sessions []string
	if newSessions {
		other, _, err := x.store.Register("codex", model.NewUUID(), t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		sessions = []string{other.ID}
	}
	ownerChangesBetweenTicks(t, x.feed, sessions...)
	c, frames := pipeChild(t)
	h := &v2Host{ctx: t.Context(), feed: x.feed, binding: &x.status, child: c, enabled: []string{"feed"}}
	if h.tick() == nil {
		t.Fatal("the old child was not stopped")
	}
	if line := firstFrame(t, frames); line != `{"code":"stale_binding"}`+"\n" {
		t.Fatalf("the old child got %q", line)
	}
	current, err := x.feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if d, err := x.feed.NextPluginFeed(&current); err != nil || d.Gap != "enabled" {
		t.Fatal("a new launch does not get the feed", d, err)
	}
}

func TestOldChildCannotSendAfterDisableAndEnable(t *testing.T) {
	x := v2HostFixture(t)
	ownerChangesBetweenTicks(t, x.feed)
	c, frames := pipeChild(t)
	service := &bus.Service{Store: x.store, PluginSend: pluginfeed.BoundPluginSend(&x.status)}
	h := &v2Host{ctx: t.Context(), feed: x.feed, service: service, binding: &x.status, child: c}
	if h.handle(&v2Command{Op: "send", RequestID: "old", IdempotencyKey: "c1", Send: &v2Send{ReplyTo: x.request.ID, To: x.asker.ID, Body: "hello"}}) == nil {
		t.Fatal("the old child was not stopped")
	}
	if line := firstFrame(t, frames); !strings.Contains(line, `"stale_binding"`) {
		t.Fatalf("the old child got %q", line)
	}
}

// TestHostRealChildGetsNoNewFeedGrant runs a real child, changes the feed
// grant from A to B while it runs, and makes a B event. The child must never
// see B, and the host must end with a named reason: disabled, or grants changed.
func TestHostRealChildGetsNoNewFeedGrant(t *testing.T) {
	store, _ := hostStore(t)
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	b, _, err := store.Register("codex", model.NewUUID(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, done := startHost(t, store)
	eventually(t, func() bool {
		s, err := feed.PluginStatus()
		return err == nil && !s.GapPending
	})
	ownerChangesBetweenTicks(t, feed, b.ID)
	if err := store.Update(b.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "plugin v2 failed") {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the old launch kept running after the grant change")
	}
	seen, err := filepath.Glob(filepath.Join(hostData(store), ".seen-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range seen {
		if raw, _ := os.ReadFile(path); strings.Contains(string(raw), b.ID) {
			t.Fatal("the old child saw an event of the new grant", string(raw))
		}
	}
}
