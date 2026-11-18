package plugins

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestPlainFeedHostCanceledAckKeepsGapAndOriginalTimer(t *testing.T) {
	store, session := hostStore(t)
	session, key, err := store.Register("codex", session.NativeID, session.CWD, nil)
	if err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	owner, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	child, reader := pipeChild(t)
	child.parent = t.Context()
	launch := captureFeedFeatures(&owner, []string{capFeed})
	host := &v2Host{ctx: t.Context(), feed: feed, binding: launch, child: child, enabled: []string{capFeed}, acks: make(chan string, 1)}
	old := preparePlainHostEvent(t, store, session.ID, host, reader)
	if err := bus.SetSessionPlugin(store, session.ID, key, "fake-notifier", true); err != nil {
		t.Fatal(err)
	}
	if err := host.ack(old); err != nil || host.offer != nil {
		t.Fatal("F_HOST_ACK_DISCOVERS_CANCELED_E_INVALIDATES_CACHED_OFFER", err)
	}
	assertCanceledPlainHostGap(t, store, host, reader, old)
}

func preparePlainHostEvent(t *testing.T, store bus.Store, id string, host *v2Host, reader *bufio.Reader) string {
	t.Helper()
	if err := host.tick(); err != nil || host.offer == nil {
		t.Fatal("initial gap", err)
	}
	firstFrame(t, reader)
	if err := host.ack(host.offer.ID); err != nil {
		t.Fatal(err)
	}
	if err := host.tick(); err != nil || host.offer != nil {
		t.Fatal("plain private cut", err)
	}
	if err := store.Update(id, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	if err := host.tick(); err != nil || host.offer != nil {
		t.Fatal("plain first-seen floor", err)
	}
	if err := store.Update(id, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
	if err := host.tick(); err != nil || host.offer == nil {
		t.Fatal("plain current event", err)
	}
	firstFrame(t, reader)
	return host.offer.ID
}

func assertCanceledPlainHostGap(t *testing.T, store bus.Store, host *v2Host, reader *bufio.Reader, old string) {
	t.Helper()
	if err := host.tick(); err != nil || host.offer == nil || host.offer.ID == old {
		t.Fatal("F_HOST_MUTED_END_REPLACED_WITH_GAP", err)
	}
	var gap struct {
		ID  string `json:"id"`
		Gap string `json:"gap"`
	}
	if json.Unmarshal([]byte(firstFrame(t, reader)), &gap) != nil || gap.Gap != "policy" || gap.ID != host.offer.ID {
		t.Fatal("F_HOST_CANCELED_END_NO_RICH_WIRE")
	}
	path := filepath.Join(store.Dir, "plugins", "fake-notifier.feed-state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	current := host.offer
	host.offerAt = time.Now().Add(-v2AckTimeout - time.Second)
	at := host.offerAt
	if err := host.ack(old); err != nil || host.offer != current || host.offerAt != at {
		t.Fatal("F_HOST_CANCELED_ACK_KEEPS_GAP_TIMER", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("F_HOST_CANCELED_ACK_NO_NEW_CHECKPOINT_EVIDENCE", err)
	}
	if host.checkFeedTimeout() == nil {
		t.Fatal("F_HOST_LATE_ACK_CANNOT_RESET_GAP_TIMEOUT")
	}
	if firstFrame(t, reader) != "{\"code\":\"protocol_error\"}\n" {
		t.Fatal("F_HOST_GAP_TIMEOUT_TERMINAL_REASON")
	}
}
