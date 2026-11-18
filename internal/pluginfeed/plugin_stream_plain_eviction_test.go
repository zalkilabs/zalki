package pluginfeed

import (
	"github.com/zalkilabs/zalki/internal/model"
	"testing"
)

func TestStreamPlainGuardEvictionErasesHistoryAndUsesFreshFloor(t *testing.T) {
	state := pluginStreamState{Rows: map[string]streamRow{}, States: map[string]streamSessionSeen{}}
	old := plainEvictionRow()
	floor := pluginPosition{Epoch: model.NewUUID(), Sequence: 10}
	state.rememberStreamRow(old, floor)
	state.States[old.ID] = streamSessionSeen{Guard: old.Guard, State: model.StateReady}
	state.Seen = []streamMessageSeen{{ID: model.NewUUID(), Guards: map[string]streamDependency{old.ID: {Guard: old.Guard, Class: "active"}}}}
	for range 64 {
		state.rememberStreamRow(plainEvictionRow(), floor)
	}
	if len(state.Rows) != 64 || len(state.RowOrder) != 64 {
		t.Fatal("F_PLAIN_GUARD_CACHE_EXACT_BOUNDED_SIZE")
	}
	if _, exists := state.Rows[old.ID]; exists || len(state.States) != 0 || len(state.Seen) != 0 {
		t.Fatal("F_PLAIN_GUARD_EVICTION_ERASES_PRIOR_AND_PARENT_PROOF")
	}
	fresh := pluginPosition{Epoch: floor.Epoch, Sequence: 20}
	state.rememberStreamRow(old, fresh)
	assertPlainReobservedFloor(t, state, old, floor, fresh)
}

func plainEvictionRow() streamRow {
	return streamRow{ID: "codex:" + model.NewUUID(), Kind: "codex", Class: "active", Guard: streamGuard{Generation: model.NewUUID(), Audit: model.NewUUID(), Owner: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
}

func assertPlainReobservedFloor(t *testing.T, state pluginStreamState, row streamRow, old, fresh pluginPosition) {
	t.Helper()
	remembered := state.Rows[row.ID]
	if remembered.Floor != fresh || remembered.Since == nil || *remembered.Since != fresh {
		t.Fatal("F_PLAIN_EVICTED_GUARD_RESTARTS_AT_CURRENT_PRIVATE_FLOOR")
	}
	current := map[string]streamRow{row.ID: remembered}
	if streamEventAllowed(state, current, old, false, row.ID) {
		t.Fatal("F_PLAIN_GUARD_LOSS_NEVER_BLESSES_OLD_INTERVAL")
	}
	later := pluginPosition{Epoch: fresh.Epoch, Sequence: fresh.Sequence + 1}
	if !streamEventAllowed(state, current, later, false, row.ID) {
		t.Fatal("F_PLAIN_REOBSERVED_POST_FLOOR_POSITIVE")
	}
	event := model.Event{Type: model.EventSessionStateChanged, Session: model.EventSession{ID: row.ID, State: model.StateBusy, Previous: model.StateReady}}
	projectStreamSession(&state, &event, &streamPending{}, row.Guard)
	if event.Session.Previous != "" {
		t.Fatal("F_PLAIN_EVICTED_PRIOR_STATE_CANNOT_LEAK")
	}
}

func TestStreamGapImmediatelyClearsPriorAndParentProof(t *testing.T) {
	row := plainEvictionRow()
	position := pluginPosition{Epoch: model.NewUUID(), Sequence: 10}
	state := pluginStreamState{Cursor: position, Rows: map[string]streamRow{row.ID: row}, RowOrder: []string{row.ID}, States: map[string]streamSessionSeen{row.ID: {Guard: row.Guard, State: model.StateReady}}, Seen: []streamMessageSeen{{ID: model.NewUUID(), From: row.ID, To: "plugin:" + model.NewUUID(), Position: position, Status: model.StatusSubmitted, Guards: map[string]streamDependency{row.ID: {Guard: row.Guard, Class: "active"}}}}}
	cut := pluginPosition{Epoch: position.Epoch, Sequence: 20}
	if err := state.queueGap("policy", cut); err != nil {
		t.Fatal(err)
	}
	if len(state.Rows) != 0 || len(state.States) != 0 || len(state.Seen) != 0 || len(state.RowOrder) != 0 {
		t.Fatal("F_GAP_CLEARS_ACK_EVIDENCE_AT_PUBLICATION")
	}
	if state.Cursor != position || state.Pending == nil || state.Pending.Kind != "gap" || state.Pending.After != cut || !state.NeedCut {
		t.Fatal("F_GAP_PUBLICATION_NEVER_ADVANCES_CURSOR_BEFORE_ACK")
	}
}
