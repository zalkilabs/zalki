package bus

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// capturingCursorAdapter records what its codec was given.
type capturingCursorAdapter struct {
	stubCursorAdapter
	seen *[]StateInput
}

func (a capturingCursorAdapter) DecodeState(in StateInput) (json.RawMessage, error) {
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

func TestStateCodecSeesOnlyFieldsCoreDoesNotOwn(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	var seen []StateInput
	registry, err := NewRegistry(stubCodexAdapter{}, capturingCursorAdapter{seen: &seen})
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
	rewriteRecord(t, store, to.ID, func(record map[string]any) {
		// encoding/json decodes every case variant into its core field, so each
		// keeps a value of that field's type (and the real key sorts after it).
		record["KEY_HASH"], record["Key_Hash"] = "core-owned", "core-owned"
		record["GENERATION"], record["ID"] = to.Generation, to.ID
		record["Listener"] = nil
		record["Adapter_State"] = map[string]int{"schema": 1}
		record["cursor_recovery_attempted"] = true
		record["client_extension"] = map[string]int{"n": 1}
	})
	if _, err := store.session(to.ID); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("codec ran %d times for one read", len(seen))
	}
	var keys []string
	for key := range seen[0].Extensions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"client_extension", "cursor_recovery_attempted"}) {
		t.Fatalf("codec saw core-owned fields: %v", keys)
	}
}

func TestStateExtensionsAreBounded(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"too many fields": func(record map[string]any) {
			for i := range maxExtensions + 1 {
				record[fmt.Sprintf("extension_%d", i)] = true
			}
		},
		"oversized field": func(record map[string]any) {
			record["extension"] = strings.Repeat("x", maxAdapterState)
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := fixtureStore(t)
			to, _ := register(t, store, "cursor")
			rewriteRecord(t, store, to.ID, edit)
			if _, err := store.session(to.ID); ErrorCodeOf(err) != CodeInvalidInput {
				t.Fatal("an unbounded record reached the adapter codec", err)
			}
		})
	}
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	rewriteRecord(t, store, to.ID, func(record map[string]any) {
		for i := range maxExtensions {
			record[fmt.Sprintf("extension_%d", i)] = true
		}
	})
	if _, err := store.session(to.ID); err != nil {
		t.Fatal("a record at the extension limit was rejected", err)
	}
}
