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
	h := &v2Host{ctx: t.Context(), feed: x.feed, binding: &x.status, child: c}
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

func TestReadHostOldChildGetsNoNewFeedGrant(t *testing.T) {
	x := v2HostFixture(t)
	other, _, err := x.store.Register("codex", model.NewUUID(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ownerChangesBetweenTicks(t, x.feed, other.ID)
	c, frames := pipeChild(t)
	h := &readHost{ctx: t.Context(), feed: x.feed, binding: &x.status, child: c}
	if h.offer() == nil {
		t.Fatal("the old child was not stopped")
	}
	if line := firstFrame(t, frames); line != `{"code":"stale_binding"}`+"\n" {
		t.Fatalf("the old child got %q", line)
	}
}

func TestV1HostAsksForARestartAfterNewGrants(t *testing.T) {
	x := v2HostFixture(t)
	other, _, err := x.store.Register("codex", model.NewUUID(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ownerChangesBetweenTicks(t, x.feed, other.ID)
	_, err = x.feed.NextPluginFeed(&x.status)
	if model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("the old launch selected a feed event", err)
	}
	if err := launchEnded(x.feed, &x.status); err == nil || err.Error() != "plugin grants changed while it ran; start it again with zalki plugins run arbiter" {
		t.Fatal("restart text", err)
	}
	if err := x.feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := launchEnded(x.feed, &x.status); err != nil {
		t.Fatal("a disabled plugin must stop quietly", err)
	}
}

func TestOldChildCannotSendAfterDisableAndEnable(t *testing.T) {
	x := v2HostFixture(t)
	ownerChangesBetweenTicks(t, x.feed)
	c, frames := pipeChild(t)
	service := &bus.Service{Store: x.store, PluginSend: pluginfeed.BoundPluginSend(&x.status)}
	h := &v2Host{ctx: t.Context(), feed: x.feed, service: service, binding: &x.status, child: c}
	if h.handle(&v2Command{Send: &v2Send{RequestID: "old", ClientMessageID: "c1", To: x.asker.ID, Body: "hello"}}) == nil {
		t.Fatal("the old child was not stopped")
	}
	if line := firstFrame(t, frames); !strings.Contains(line, `"stale_binding"`) {
		t.Fatalf("the old child got %q", line)
	}
}

// TestV1HostRealChildGetsNoNewFeedGrant runs a real v1 child, changes the feed
// grant from A to B while it runs, and makes a B event. The child must never
// see B, and the host must end: quietly if it saw the disabled moment, or
// with the restart text if it did not.
func TestV1HostRealChildGetsNoNewFeedGrant(t *testing.T) {
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
		if err != nil && !strings.HasPrefix(err.Error(), "plugin grants changed while it ran; start it again with zalki plugins run fake-notifier") {
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
