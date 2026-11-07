package bus

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

// Keep the cross-actor audit strict while admitting the one expected outbox
// event for a successful, bound ready-to-busy update.
func allowBoundStateEvent(t *testing.T, store Store, target Session, before map[string]string) {
	t.Helper()
	metadata := eventState(t, store)
	if metadata.Count != 1 || metadata.First != 1 || metadata.Last != 1 {
		t.Fatalf("unexpected bound event count: %+v", metadata)
	}
	var record eventRecord
	raw := readCommitFile(t, filepath.Join(store.Dir, eventRecordPath(1)))
	if err := decodeEventJSON(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Event.Session.ID != target.ID || record.Event.Session.Generation != target.Generation || record.Event.Session.Previous != StateReady || record.Event.Session.State != StateBusy {
		t.Fatal("event was not bound to the updated session")
	}
	var expected stateFormats
	if err := json.Unmarshal([]byte(before[stateFormatFile]), &expected); err != nil {
		t.Fatal(err)
	}
	actual, _, err := store.readStateFormats()
	if err != nil || !reflect.DeepEqual(expected, actual) {
		t.Fatalf("format changes exceeded declared event families: %+v, %v", actual, err)
	}
	for _, path := range []string{eventRecordPath(1), eventMetaPath} {
		before[path] = string(readCommitFile(t, filepath.Join(store.Dir, path)))
	}
}
