package pluginfeed

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamAttemptedContinuationPrecedesLaterJournalLoss(t *testing.T) {
	for _, kind := range []string{"event"} {
		for _, reason := range []string{"retention", "epoch"} {
			t.Run(kind+"/"+reason, func(t *testing.T) { checkAttemptedContinuationLoss(t, kind, reason) })
		}
	}
}

func checkAttemptedContinuationLoss(t *testing.T, kind, reason string) {
	t.Helper()
	feed, launch, session, _, offer := streamPendingMessageFixture(t)
	sessionID := session.ID
	part, err := feed.NextPluginFeed(t.Context(), launch, offer.ID, 1)
	if err != nil || part.ID != offer.ID || len(part.Frames[0]) == 0 {
		t.Fatal("initial attempted part", err)
	}
	saved := readStreamCheckpoint(t, feed)
	expected := saved.Pending
	if reason == "retention" {
		for range 3 {
			testCoveredUpdate(t, feed.store, sessionID)
		}
	} else {
		head := eventState(t, feed.store)
		head.Epoch = model.NewUUID()
		if err := writeJSON(filepath.Join(feed.store.Dir(), eventMetaPath), head); err != nil {
			t.Fatal(err)
		}
	}
	restarted := NewPluginFeed(feed.store, feed.name)
	freezeStreamWork(restarted)
	replay := streamInspect(t, restarted, launch)
	if replay.ID != offer.ID || replay.Gap != "" {
		t.Fatal("F_ATTEMPTED_CONTINUATION_BEFORE_LATER_LOSS", kind, reason, replay)
	}
	for i, raw := range streamAttemptAll(t, restarted, launch, replay) {
		if !bytes.Equal(raw, expected.Frames[i]) {
			t.Fatal("F_ATTEMPTED_LOSS_REPLAY_EXACT_IMMUTABLE_BYTES")
		}
	}
	if readStreamCheckpoint(t, restarted).Cursor != saved.Cursor {
		t.Fatal("F_ATTEMPTED_LOSS_NO_OPTIMISTIC_CURSOR")
	}
	if _, err := restarted.AckPluginFeed(launch, replay.ID); err != nil {
		t.Fatal(err)
	}
	assertAttemptedLossReset(t, restarted, launch, offer.ID)
}

func assertAttemptedLossReset(t *testing.T, restarted PluginFeed, launch *PluginFeedStatus, oldID string) {
	t.Helper()
	next := streamInspect(t, restarted, launch)
	var frame streamFrame
	if next.ID == "" || next.ID == oldID || next.Gap == "" || next.Event != nil {
		t.Fatal("F_ATTEMPTED_LOSS_FOLLOWED_BY_EXPLICIT_RESET", next)
	}
	raw := streamAttemptAll(t, restarted, launch, next)
	if len(raw) != 1 || json.Unmarshal(raw[0], &frame) != nil || frame.Gap == "" {
		t.Fatal("F_ATTEMPTED_LOSS_NEVER_FALSE_COMPLETE_HISTORY")
	}
}

func TestStreamPreparedHistoryCannotUseAttemptedContinuation(t *testing.T) {
	feed, launch, _, _, offer := streamPendingMessageFixture(t)
	before := readStreamCheckpoint(t, feed)
	if before.Pending == nil || before.Pending.Attempted {
		t.Fatal("prepared fixture")
	}
	head := eventState(t, feed.store)
	head.Epoch = model.NewUUID()
	if err := writeJSON(filepath.Join(feed.store.Dir(), eventMetaPath), head); err != nil {
		t.Fatal(err)
	}
	restarted := NewPluginFeed(feed.store, feed.name)
	freezeStreamWork(restarted)
	reset := streamInspect(t, restarted, launch)
	if reset.ID == offer.ID || reset.ID == "" || reset.Gap != "launch" || reset.Event != nil {
		t.Fatal("F_UNATTEMPTED_HISTORY_NEVER_USES_REPLAY_EXCEPTION", reset)
	}
	if state := readStreamCheckpoint(t, restarted); state.Cursor != before.Cursor || len(state.Seen) != 0 || len(state.Rows) != 0 {
		t.Fatal("F_UNATTEMPTED_RESTART_NO_CURSOR_OR_EVIDENCE_CREDIT")
	}
}
