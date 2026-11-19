package pluginfeed

import (
	"bytes"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamRetirePromptReactivateBetweenPollsForcesPolicyReset(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "cursor")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	if err := store.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	input := `{"session_id":"` + session.NativeID + `","cwd":"/workspace","hook_event_name":"beforeSubmitPrompt"}`
	service := &bus.Service{Store: store}
	if err := service.Hook(t.Context(), "cursor", bytes.NewBufferString(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err := bus.WithJournalRead(store, func(view bus.JournalView) error {
		var err error
		generation, err = view.SessionGeneration(session.ID)
		return err
	}); err != nil || generation != session.Generation {
		t.Fatal("F_NATIVE_ABA_REACTIVATION_PRESERVES_GENERATION", generation, err)
	}
	delivery, frame := streamTake(t, feed, launch)
	if delivery.ID == "" || frame.Gap != "policy" || frame.Event != nil {
		t.Fatal("F_NATIVE_RETIRE_REACTIVATE_ABA_RESETS_BEFORE_HISTORY")
	}
}

func TestStreamOrdinaryStateAndTaskUpdatesDoNotResyncPolicy(t *testing.T) {
	store := fixtureStore(t)
	session, key := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	guard := readStreamCheckpoint(t, feed).Rows[session.ID].Guard
	for _, state := range []model.SessionState{model.StateBusy, model.StateReady} {
		updateStreamState(t, store, session.ID, state)
		delivery, frame := streamTake(t, feed, launch)
		if frame.Event == nil || frame.Gap != "" || frame.Event.Session.State != state {
			t.Fatal("F_NATIVE_ORDINARY_UPDATES_NEVER_POLICY_RESYNC")
		}
		acknowledgeStream(t, feed, launch, delivery.ID)
	}
	if _, err := (&bus.Service{Store: store}).SetTask(session.ID, key, "current task"); err != nil {
		t.Fatal(err)
	}
	updateStreamState(t, store, session.ID, model.StateBusy)
	delivery, frame := streamTake(t, feed, launch)
	if frame.Gap != "" || frame.Event == nil || frame.Event.Session.State != model.StateBusy {
		t.Fatal("F_NATIVE_ORDINARY_UPDATES_NEVER_POLICY_RESYNC")
	}
	acknowledgeStream(t, feed, launch, delivery.ID)
	if readStreamCheckpoint(t, feed).Rows[session.ID].Guard != guard {
		t.Fatal("F_NATIVE_ORDINARY_UPDATES_KEEP_AUDIT_GUARD")
	}
}
