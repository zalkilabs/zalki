package pluginfeed

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func streamPendingMessageFixture(t *testing.T) (PluginFeed, *PluginFeedStatus, model.Session, string, PluginDelivery) {
	t.Helper()
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, from.ID)
	primeStreamSession(t, store, feed, launch, to.ID)
	if _, _, _, err := store.ReserveMessage(bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: model.NewUUID(), Body: "no body in checkpoint metadata"}); err != nil {
		t.Fatal(err)
	}
	offer := streamInspect(t, feed, launch)
	if offer.ID == "" {
		t.Fatal("message fixture has no prepared event")
	}
	return feed, launch, from, key, offer
}

func TestStreamReceiptRequiresSavedNativeGuards(t *testing.T) {
	for _, kind := range []string{"handoff", "ack"} {
		t.Run(kind, func(t *testing.T) { checkStreamReceiptNativeGuards(t, kind) })
	}
}

func checkStreamReceiptNativeGuards(t *testing.T, kind string) {
	t.Helper()
	feed, launch, from, key, offer := streamPendingMessageFixture(t)
	if kind == "ack" {
		streamAttemptAll(t, feed, launch, offer)
	}
	state := readStreamCheckpoint(t, feed)
	state.Pending.Guards = nil
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(feed.streamPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := bus.SetSessionPlugin(feed.store, from.ID, key, launch.Name, true); err != nil {
		t.Fatal(err)
	}
	if kind == "handoff" {
		current, err := feed.NextPluginFeed(t.Context(), launch, offer.ID, 1)
		if err == nil && current.ID == offer.ID && len(current.Frames) > 0 && len(current.Frames[0]) > 0 {
			t.Fatal("F_CHECKPOINT_MISSING_ORIGINAL_GUARDS_NO_HANDOFF")
		}
	} else {
		if _, err := feed.AckPluginFeed(launch, offer.ID); err == nil {
			t.Fatal("F_CHECKPOINT_MISSING_ORIGINAL_GUARDS_NO_ACK")
		}
	}
	after, err := os.ReadFile(feed.streamPath())
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("F_CHECKPOINT_BAD_PROVENANCE_NO_OPTIMISTIC_REPAIR", err)
	}
}

func TestStreamAckRevokedAndStaleEpochHaveDistinctCodes(t *testing.T) {
	for _, kind := range []string{"revoked", "stale-epoch"} {
		t.Run(kind, func(t *testing.T) {
			feed, launch, _, _, offer := streamPendingMessageFixture(t)
			streamAttemptAll(t, feed, launch, offer)
			before, err := os.ReadFile(feed.streamPath())
			if err != nil {
				t.Fatal(err)
			}
			want := model.CodeUnauthorized
			if kind == "revoked" {
				err = feed.RevokePluginAccess()
			} else {
				err = feed.ConfigurePluginScope("none")
				want = model.CodeConflict
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := feed.AckPluginFeed(launch, offer.ID); model.ErrorCodeOf(err) != want {
				t.Fatal("F_ACK_REVOKED_UNAUTHORIZED_STALE_EPOCH_CONFLICT", kind, err)
			}
			after, err := os.ReadFile(feed.streamPath())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("F_ACK_BINDING_FAILURE_CHECKPOINT_UNCHANGED", err)
			}
		})
	}
}

func TestStreamLastAckNoOpPreservesDistinctGapAndCheckpoint(t *testing.T) {
	feed, launch, from, key, offer := streamPendingMessageFixture(t)
	streamAttemptAll(t, feed, launch, offer)
	acknowledgeStream(t, feed, launch, offer.ID)
	if err := bus.SetSessionPlugin(feed.store, from.ID, key, launch.Name, true); err != nil {
		t.Fatal(err)
	}
	updateStreamState(t, feed.store, from.ID, model.StateBusy)
	gap := streamInspect(t, feed, launch)
	if gap.ID == "" || gap.ID == offer.ID || gap.Gap != "policy" {
		t.Fatal("F_ACK_NOOP_HAS_DISTINCT_PENDING_GAP")
	}
	before, err := os.ReadFile(feed.streamPath())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := feed.AckPluginFeed(launch, offer.ID)
	if err != nil || pending != gap.ID {
		t.Fatal("F_ACK_LAST_COMMITTED_EXACT_NOOP_KEEPS_GAP", err)
	}
	after, err := os.ReadFile(feed.streamPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("F_ACK_NOOP_CURSOR_TABLE_EVIDENCE_BYTES_UNCHANGED", err)
	}
	if _, err := feed.AckPluginFeed(launch, model.NewUUID()); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("F_ACK_UNKNOWN_ID_REMAINS_CONFLICT", err)
	}
}
