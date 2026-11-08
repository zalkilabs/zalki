package bus

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// Keep the cross-actor audit strict while admitting the one expected outbox
// event for a successful, bound ready-to-busy update.
func allowBoundStateEvent(t *testing.T, store Store, target model.Session, before map[string]string) {
	t.Helper()
	var prior eventMetadata
	if err := json.Unmarshal([]byte(before[eventMetaPath]), &prior); err != nil {
		t.Fatal(err)
	}
	metadata := eventState(t, store)
	if metadata.Count != prior.Count+1 || metadata.First != prior.First || metadata.Last != prior.Last+1 {
		t.Fatalf("unexpected bound event count: %+v", metadata)
	}
	var record eventRecord
	raw := readCommitFile(t, filepath.Join(store.Dir, eventRecordPath(metadata.Last)))
	if err := decodeEventJSON(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Event.Session.ID != target.ID || record.Event.Session.Generation != target.Generation || record.Event.Session.Previous != model.StateReady || record.Event.Session.State != model.StateBusy {
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
	for _, path := range []string{eventRecordPath(metadata.Last), eventMetaPath} {
		before[path] = string(readCommitFile(t, filepath.Join(store.Dir, path)))
	}
}
