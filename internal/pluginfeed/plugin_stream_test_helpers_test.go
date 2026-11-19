package pluginfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func streamTestLaunch(t *testing.T, feed PluginFeed) *PluginFeedStatus {
	t.Helper()
	freezeStreamWork(feed)
	status, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.FeedFeatures.Feed = true
	return &status
}

func freezeStreamWork(feed PluginFeed) {
	frozen := time.Now()
	feed.stream.now = func() time.Time { return frozen }
	feed.stream.budget = func(ctx context.Context) *streamBudget {
		budget := newStreamBudget(ctx)
		at := budget.deadline.Add(-time.Millisecond)
		budget.now = func() time.Time { return at }
		return budget
	}
}

func streamInspect(t *testing.T, feed PluginFeed, launch *PluginFeedStatus) PluginDelivery {
	t.Helper()
	delivery, err := inspectFixtureStream(t.Context(), feed, launch)
	if err != nil {
		t.Fatal(err)
	}
	return delivery
}

func streamAttemptAll(t *testing.T, feed PluginFeed, launch *PluginFeedStatus, delivery PluginDelivery) [][]byte {
	t.Helper()
	parts := make([][]byte, 0, len(delivery.Frames))
	for i := range delivery.Frames {
		current, err := feed.NextPluginFeed(t.Context(), launch, delivery.ID, i+1)
		if err != nil || current.ID != delivery.ID || len(current.Frames[i]) == 0 {
			raw, _ := os.ReadFile(feed.streamPath())
			var state pluginStreamState
			_ = json.Unmarshal(raw, &state)
			if state.Pending != nil {
				var stored streamFrame
				_ = json.Unmarshal(state.Pending.Frames[0], &stored)
				t.Log("checkpoint kind", state.Pending.Kind, "gap", stored.Gap, "backoff", state.Backoff)
			}
			t.Fatal("F_RUNTIME_PREFLIGHT_EXACT_PART", "part", i+1, "parts", len(delivery.Frames), "expected", delivery.ID, "current", current.ID, "gap", current.Gap, "frames", len(current.Frames), err)
		}
		parts = append(parts, bytes.Clone(current.Frames[i]))
	}
	return parts
}

func streamComplete(t *testing.T, feed PluginFeed, launch *PluginFeedStatus, delivery PluginDelivery) {
	t.Helper()
	streamAttemptAll(t, feed, launch, delivery)
	acknowledgeStream(t, feed, launch, delivery.ID)
}

func streamReady(t *testing.T, feed PluginFeed, launch *PluginFeedStatus) {
	t.Helper()
	gap := streamInspect(t, feed, launch)
	if gap.ID == "" {
		t.Fatal("F_RUNTIME_INITIAL_GAP")
	}
	streamComplete(t, feed, launch, gap)
	if cut := streamInspect(t, feed, launch); cut.ID != "" {
		t.Fatal("B_PLAIN_PRIVATE_CUT_HAS_NO_PUBLIC_ROWS")
	}
}

func readStreamCheckpoint(t *testing.T, feed PluginFeed) pluginStreamState {
	t.Helper()
	state, exists, err := feed.loadStreamState(feed.stream.budget(t.Context()))
	if err != nil || !exists {
		t.Fatal(err)
	}
	return state
}

func streamTake(t *testing.T, feed PluginFeed, launch *PluginFeedStatus) (PluginDelivery, streamFrame) {
	t.Helper()
	var delivery PluginDelivery
	for range 4 {
		delivery = streamInspect(t, feed, launch)
		if delivery.ID != "" {
			break
		}
	}
	if delivery.ID == "" {
		t.Fatal("F_RUNTIME_EXPECTED_REAL_FEED_FACT")
	}
	parts := streamAttemptAll(t, feed, launch, delivery)
	var frame streamFrame
	if len(parts) != 1 || json.Unmarshal(parts[0], &frame) != nil {
		t.Fatal("expected one frame")
	}
	return delivery, frame
}

func primeStreamSession(t *testing.T, store bus.Store, feed PluginFeed, launch *PluginFeedStatus, id string) {
	t.Helper()
	path, err := sessionTestPath(store, id)
	if err != nil {
		t.Fatal(err)
	}
	var current model.Session
	if err := json.Unmarshal(readCommitFile(t, path), &current); err != nil {
		t.Fatal(err)
	}
	first := model.StateBusy
	if current.State == model.StateBusy {
		first = model.StateReady
	}
	updateStreamState(t, store, id, first)
	if unseen := streamInspect(t, feed, launch); unseen.ID != "" {
		t.Fatal("B_PLAIN_UNSEEN_INTERVAL_HAS_PRIVATE_FLOOR", unseen)
	}
	updateStreamState(t, store, id, current.State)
	offer, frame := streamTake(t, feed, launch)
	if frame.Event == nil || frame.Event.Session.ID != id || frame.Event.Session.State != current.State {
		t.Fatal("B_PLAIN_POST_FLOOR_EVENT_POSITIVE")
	}
	acknowledgeStream(t, feed, launch, offer.ID)
}

// These helpers preserve the actual production API call and its strict
// success assertion; boundary-specific code/byte assertions stay at the callsite.
func acknowledgeStream(t *testing.T, feed PluginFeed, launch *PluginFeedStatus, id string) {
	t.Helper()
	if _, err := feed.AckPluginFeed(launch, id); err != nil {
		t.Fatal(err)
	}
}

func updateStreamState(t *testing.T, store bus.Store, id string, state model.SessionState) {
	t.Helper()
	if err := store.Update(id, state, false); err != nil {
		t.Fatal(err)
	}
}
