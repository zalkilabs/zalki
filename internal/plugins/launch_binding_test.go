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
// a running child: disable, optionally regrant scope/access, enable again.
func ownerChangesBetweenTicks(t *testing.T, feed pluginfeed.PluginFeed, regrant bool) {
	t.Helper()
	if err := feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if regrant {
		if err := feed.ConfigurePluginScope("all"); err != nil {
			t.Fatal(err)
		}
		if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true}); err != nil {
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
	return &child{input: wr, parent: t.Context()}, bufio.NewReader(rd)
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
	captured := captureFeedFeatures(&x.status, []string{capFeed})
	if newSessions {
		if _, _, err := x.store.Register("codex", model.NewUUID(), t.TempDir(), nil); err != nil {
			t.Fatal(err)
		}
	}
	ownerChangesBetweenTicks(t, x.feed, newSessions)
	c, frames := pipeChild(t)
	h := &v2Host{ctx: t.Context(), feed: x.feed, binding: captured, child: c, enabled: []string{"feed"}}
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
	if d, err := testFeedNext(x.feed, &current); err != nil || d.Gap != "enabled" {
		t.Fatal("a new launch does not get the feed", d, err)
	}
}

func TestOldChildCannotSendAfterDisableAndEnable(t *testing.T) {
	x := v2HostFixture(t)
	ownerChangesBetweenTicks(t, x.feed, false)
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

// TestHostRealChildGetsNoNewFeedGrant runs a real child and regrants its feed.
// Only then is B registered and updated, so every B event belongs to the new
// grant. The old child must never see B and must end after the owner change.
func TestHostRealChildGetsNoNewFeedGrant(t *testing.T) {
	store, _ := hostStore(t)
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	_, done := startHost(t, store)
	eventually(t, func() bool {
		s, err := feed.PluginStatus()
		return err == nil && !s.GapPending
	})
	ownerChangesBetweenTicks(t, feed, true)
	b, _, err := store.Register("codex", model.NewUUID(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
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
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("read the old child's recorded frame", err)
		}
		var value frame
		if err := decodeFrame(raw, &value); err != nil {
			t.Fatal("decode the old child's recorded frame", err)
		}
		if strings.Contains(string(raw), b.ID) {
			t.Fatal("the old child saw an event of the new grant", string(raw))
		}
	}
}
