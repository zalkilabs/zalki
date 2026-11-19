package pluginfeed

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamPlainPreviousStateRequiresAckInCurrentView(t *testing.T) {
	store := fixtureStore(t)
	session, key := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	updateStreamState(t, store, session.ID, model.StateBusy)
	if unseen := streamInspect(t, feed, launch); unseen.ID != "" {
		t.Fatal("F_PLAIN_FIRST_INTERVAL_PRIVATE_FLOOR")
	}
	updateStreamState(t, store, session.ID, model.StateReady)
	first, frame := streamTake(t, feed, launch)
	assertPlainPreviousState(t, frame, model.StateReady, "")
	acknowledgeStream(t, feed, launch, first.ID)
	updateStreamState(t, store, session.ID, model.StateBusy)
	second, frame := streamTake(t, feed, launch)
	assertPlainPreviousState(t, frame, model.StateBusy, model.StateReady)
	acknowledgeStream(t, feed, launch, second.ID)
	for _, muted := range []bool{true, false} {
		if err := bus.SetSessionPlugin(store, session.ID, key, launch.Name, muted); err != nil {
			t.Fatal(err)
		}
	}
	updateStreamState(t, store, session.ID, model.StateReady)
	gap, frame := streamTake(t, feed, launch)
	if frame.Gap != "policy" || frame.Event != nil {
		t.Fatal("F_PLAIN_HIDDEN_TOGGLE_RESETS_PREVIOUS_PROOF")
	}
	acknowledgeStream(t, feed, launch, gap.ID)
	assertPlainPostResetPrevious(t, store, feed, launch, session.ID)
}

func assertPlainPostResetPrevious(t *testing.T, store bus.Store, feed PluginFeed, launch *PluginFeedStatus, id string) {
	t.Helper()
	if cut := streamInspect(t, feed, launch); cut.ID != "" {
		t.Fatal("F_PLAIN_RESET_HAS_NO_PUBLIC_ROWS")
	}
	updateStreamState(t, store, id, model.StateBusy)
	if unseen := streamInspect(t, feed, launch); unseen.ID != "" {
		t.Fatal("F_PLAIN_POST_RESET_INTERVAL_PRIVATE_FLOOR")
	}
	updateStreamState(t, store, id, model.StateReady)
	offer, frame := streamTake(t, feed, launch)
	assertPlainPreviousState(t, frame, model.StateReady, "")
	acknowledgeStream(t, feed, launch, offer.ID)
}

func assertPlainPreviousState(t *testing.T, frame streamFrame, state, previous model.SessionState) {
	t.Helper()
	if frame.Event == nil || frame.Event.Message != nil || frame.Event.Session.State != state || frame.Event.Session.Previous != previous || frame.Event.Session.PreviousRetired {
		t.Fatal("F_PLAIN_PREVIOUS_STATE_REQUIRES_CURRENT_VIEW_ACK")
	}
}

func TestStreamSessionProjectionOmitsUnseenPreviousBeforePersistence(t *testing.T) {
	id := "codex:" + model.NewUUID()
	guard := streamGuard{Generation: model.NewUUID(), Audit: model.NewUUID(), Owner: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	state := pluginStreamState{States: map[string]streamSessionSeen{}}
	event := model.Event{Type: model.EventSessionStateChanged, Session: model.EventSession{ID: id, Generation: guard.Generation, State: model.StateBusy, Previous: model.StateReady, PreviousRetired: true}}
	projectStreamSession(&state, &event, &streamPending{}, guard)
	if event.Session.Previous != "" || event.Session.PreviousRetired {
		t.Fatal("F_NATIVE_UNSEEN_PREVIOUS_OMITTED_BEFORE_PERSISTENCE")
	}
	state.States[id] = streamSessionSeen{Guard: guard, State: model.StateReady}
	event.Session.Previous = model.StateReady
	event.Session.PreviousRetired = false
	projectStreamSession(&state, &event, &streamPending{}, guard)
	if event.Session.Previous != model.StateReady || event.Session.PreviousRetired {
		t.Fatal("F_NATIVE_ACKED_PREVIOUS_STATE_POSITIVE")
	}
}
