package pluginfeed

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamDirectRetirementUsesExactSourceWitnessAndEndsView(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	if err := store.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	delivery, frame := streamTake(t, feed, launch)
	if frame.Event == nil || frame.Event.Session.ID != session.ID || !frame.Event.Session.Retired || frame.Event.Session.Cause == model.CauseOwnerExited {
		t.Fatal("F_RETIREMENT_DIRECT_WITNESS_RICH_POSITIVE")
	}
	acknowledgeStream(t, feed, launch, delivery.ID)
	state := readStreamCheckpoint(t, feed)
	if len(state.Rows) != 0 || len(state.States) != 0 || state.Pending == nil || state.LastGap != "membership" {
		t.Fatal("F_RETIREMENT_FINAL_FACT_GRANTS_NO_LATER_VIEW")
	}
	before := readCommitFile(t, feed.streamPath())
	if pending, err := feed.AckPluginFeed(launch, delivery.ID); err != nil || pending != state.Pending.ID {
		t.Fatal("F_RETIREMENT_LOGICAL_ONCE_KEEPS_NEW_GAP", err)
	}
	if string(before) != string(readCommitFile(t, feed.streamPath())) {
		t.Fatal("F_RETIREMENT_REPEATED_ACK_EXACT_NOOP")
	}
}

func TestStreamHiddenToggleThenRetirementCannotUseNewWitness(t *testing.T) {
	store := fixtureStore(t)
	session, key := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	for _, muted := range []bool{true, false} {
		if err := bus.SetSessionPlugin(store, session.ID, key, launch.Name, muted); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	delivery, frame := streamTake(t, feed, launch)
	if frame.Event != nil || frame.Gap != "policy" || delivery.ID == "" {
		t.Fatal("F_RETIREMENT_HIDDEN_ABA_HAS_NO_LAST_ACK_PROOF")
	}
}

func TestStreamOwnerExitExcludedEvenWithOtherwiseValidRetirementProof(t *testing.T) {
	proof := &streamRetirement{Event: model.NewUUID(), Before: streamGuard{Generation: model.NewUUID(), Audit: model.NewUUID(), Owner: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BeforeDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	proof.After = proof.Before
	proof.After.Audit = model.NewUUID()
	event := model.Event{ID: proof.Event, Type: model.EventSessionStateChanged, Session: model.EventSession{Generation: proof.Before.Generation, Retired: true}}
	if !validStreamRetirement(proof, event) {
		t.Fatal("F_RETIREMENT_STATIC_VALID_PROOF_POSITIVE")
	}
	for _, retired := range []bool{false, true} {
		event.Session.Retired, event.Session.Cause = retired, model.CauseOwnerExited
		if validStreamRetirement(proof, event) {
			t.Fatal("F_RETIREMENT_OWNER_EXIT_NEVER_RICH_EITHER_FLAG", retired)
		}
	}
}

func TestStreamRetirementCandidateMustMatchLastAckedGuard(t *testing.T) {
	proof := &streamRetirement{Event: model.NewUUID(), Before: streamGuard{Generation: model.NewUUID(), Audit: model.NewUUID(), Owner: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BeforeDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	proof.After = proof.Before
	proof.After.Audit = model.NewUUID()
	id := "codex:" + model.NewUUID()
	position := pluginPosition{Epoch: model.NewUUID(), Sequence: 2}
	floor := position
	floor.Sequence = 1
	state := pluginStreamState{Rows: map[string]streamRow{id: {Class: "active", Guard: proof.Before, Floor: floor}}, States: map[string]streamSessionSeen{id: {Guard: proof.Before, State: model.StateBusy}}}
	event := model.Event{ID: proof.Event, Type: model.EventSessionStateChanged, Session: model.EventSession{ID: id, Generation: proof.Before.Generation, Retired: true}}
	row := streamRow{ID: id, Class: "absent", Guard: proof.After, Digest: proof.Digest, Retirement: proof}
	if streamRetirementCandidate(state, event, row, position) == nil {
		t.Fatal("F_RETIREMENT_LAST_ACK_DIRECT_POSITIVE")
	}
	proof.Before.Audit = model.NewUUID()
	if streamRetirementCandidate(state, event, row, position) != nil {
		t.Fatal("F_RETIREMENT_CANDIDATE_REQUIRES_LAST_ACK_GUARD")
	}
}
