package bus

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// capturingCursorAdapter records what its codec was given.
type capturingCursorAdapter struct {
	stubCursorAdapter
	seen *[]agent.StateInput
}

func (a capturingCursorAdapter) DecodeState(in agent.StateInput) (json.RawMessage, error) {
	*a.seen = append(*a.seen, in)
	return a.stubCursorAdapter.DecodeState(in)
}

func rewriteRecord(t *testing.T, store Store, id string, edit func(map[string]any)) {
	t.Helper()
	path, _ := store.sessionPath(id)
	var record map[string]any
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	edit(record)
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
}

// The codec sees only the adapter's own state. Top-level fields core does not
// own are not passed on (and are dropped on the next write).
func TestStateCodecSeesOnlyTheAdapterState(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	var seen []agent.StateInput
	registry, err := agent.NewRegistry(stubCodexAdapter{}, capturingCursorAdapter{seen: &seen})
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
	rewriteRecord(t, store, to.ID, func(record map[string]any) {
		record["adapter_state"] = map[string]any{"schema": 1, "recovery_attempted": true}
		record["cursor_recovery_attempted"] = true
		record["client_extension"] = map[string]int{"n": 1}
	})
	if _, err := store.session(to.ID); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("codec ran %d times for one read", len(seen))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, seen[0].State); err != nil || compact.String() != `{"recovery_attempted":true,"schema":1}` {
		t.Fatalf("codec input = %s, want only the adapter state", seen[0].State)
	}
}
