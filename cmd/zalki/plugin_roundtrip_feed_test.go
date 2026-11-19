package main

import (
	"encoding/json"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"os"
	"path/filepath"
	"testing"
)

type ownerFeedPosition struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence,string"`
}
type ownerFeedCheckpoint struct {
	Cursor  ownerFeedPosition `json:"cursor"`
	LastGap string            `json:"last_gap"`
	NeedCut bool              `json:"need_cut"`
	Pending *json.RawMessage  `json:"pending"`
	Rows    map[string]struct {
		Class string            `json:"class"`
		Floor ownerFeedPosition `json:"floor"`
	} `json:"rows"`
}

func readOwnerFeedCheckpoint(t *testing.T, state string) ownerFeedCheckpoint {
	t.Helper()
	raw, err := journal.ReadBytes(filepath.Join(state, "plugins", "fake-notifier.feed-state.json"), 524288)
	if err != nil {
		t.Fatal("F_CLI_BOUNDED_PRIVATE_CHECKPOINT_READ", err)
	}
	var checkpoint ownerFeedCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal("F_CLI_BOUNDED_PRIVATE_CHECKPOINT_VALID", err)
	}
	return checkpoint
}
func assertOwnerNoFeedEvents(t *testing.T, outbox string) {
	t.Helper()
	files, err := os.ReadDir(outbox)
	if err != nil || len(files) != 0 {
		t.Fatal("F_CLI_PLAIN_FIRST_UNKNOWN_INTERVAL_HAS_NO_EVENT", files, err)
	}
}
func awaitPluginOwnerInitialCut(t *testing.T, state string) {
	t.Helper()
	ready := func() bool {
		checkpoint := readOwnerFeedCheckpoint(t, state)
		return checkpoint.LastGap == "enabled" && checkpoint.Pending == nil && !checkpoint.NeedCut && len(checkpoint.Rows) == 0
	}
	pluginOwnerEventually(t, ready)
	if !ready() {
		t.Fatal("F_CLI_INITIAL_PRIVATE_CUT_PRECEDES_READY")
	}
}
func awaitPluginOwnerFloor(t *testing.T, state, outbox, session string) {
	t.Helper()
	pluginOwnerEventually(t, func() bool {
		assertOwnerNoFeedEvents(t, outbox)
		checkpoint := readOwnerFeedCheckpoint(t, state)
		row, exists := checkpoint.Rows[session]
		return exists && row.Class == "active" && model.IsCanonicalUUID(row.Floor.Epoch) && row.Floor.Sequence > 0 && checkpoint.Pending == nil && !checkpoint.NeedCut
	})
	assertOwnerNoFeedEvents(t, outbox)
}
func pluginOwnerNativeBusyEvent(t *testing.T, store bus.Store, session model.Session) (model.Event, ownerFeedPosition) {
	t.Helper()
	var event model.Event
	var head bus.EventWindow
	err := bus.WithJournalRead(store, func(view bus.JournalView) error {
		var err error
		head, err = view.EventWindow()
		if err != nil {
			return err
		}
		event, err = view.EventAt(head.Epoch, head.Last)
		return err
	})
	if err != nil {
		t.Fatal("F_CLI_NATIVE_EVENT_USES_PRODUCTION_READER", err)
	}
	if event.Type != model.EventSessionStateChanged || event.Message != nil || event.Session.ID != session.ID || event.Session.Generation != session.Generation || event.Session.State != model.StateBusy || event.Session.Previous != model.StateReady || event.Session.Retired || event.Session.PreviousRetired {
		t.Fatal("F_CLI_NATIVE_BUSY_FACT_EXACT", event)
	}
	return event, ownerFeedPosition{head.Epoch, head.Last}
}
func pluginOwnerSameEventIdentity(got, want model.Event) bool {
	return got.ID == want.ID && got.Type == want.Type && got.OccurredAt == want.OccurredAt && got.TransactionID == want.TransactionID && got.Message == nil && want.Message == nil
}
func pluginOwnerSameSessionFact(got, want model.EventSession) bool {
	return got.ID == want.ID && got.Generation == want.Generation && got.State == want.State && got.Retired == want.Retired && got.Cause == want.Cause && got.Previous == "" && !got.PreviousRetired
}
func awaitPluginOwnerRestartCut(t *testing.T, state string, expected ownerFeedPosition) {
	t.Helper()
	pluginOwnerEventually(t, func() bool {
		checkpoint := readOwnerFeedCheckpoint(t, state)
		return checkpoint.LastGap == "launch" && checkpoint.Pending == nil && !checkpoint.NeedCut && len(checkpoint.Rows) == 0 && checkpoint.Cursor == expected
	})
}
