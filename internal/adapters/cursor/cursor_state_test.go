package cursor

import (
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestCursorStateMigratesLegacyFieldsAndDropsPreLeaseWake(t *testing.T) {
	out, err := (Adapter{}).DecodeState(bus.StateInput{Extensions: map[string]json.RawMessage{
		"cursor_recovery_attempted":  json.RawMessage(`true`),
		"cursor_recovery_suppressed": json.RawMessage(`true`),
		"cursor_wake":                json.RawMessage(`{"path":"/tmp/zalki-old/inbox.sock"}`),
	}})
	if err != nil || string(out) != `{"schema":1,"recovery_attempted":true,"recovery_suppressed":true}` {
		t.Fatalf("legacy fields were not migrated exactly: %s %v", out, err)
	}
	out, err = (Adapter{}).DecodeState(bus.StateInput{State: json.RawMessage(`{"schema":1,"recovery_attempted":true,"wake":{"path":"/tmp/zalki-old/inbox.sock"}}`)})
	if err != nil || string(out) != `{"schema":1,"recovery_attempted":true}` {
		t.Fatalf("a pre-lease wake survived normalization: %s %v", out, err)
	}
	if out, err := (Adapter{}).DecodeState(bus.StateInput{}); err != nil || string(out) != `{"schema":1}` {
		t.Fatalf("a record without state did not get the current schema: %s %v", out, err)
	}
}

func TestCursorStateRejectsUnknownSchemasAndMalformedLegacyFields(t *testing.T) {
	for name, in := range map[string]bus.StateInput{
		"future schema":   {State: json.RawMessage(`{"schema":2}`)},
		"missing schema":  {State: json.RawMessage(`{"recovery_attempted":true}`)},
		"malformed state": {State: json.RawMessage(`{"schema":`)},
		"malformed legacy": {Extensions: map[string]json.RawMessage{
			"cursor_recovery_attempted": json.RawMessage(`"yes"`),
		}},
	} {
		if out, err := (Adapter{}).DecodeState(in); err == nil {
			t.Fatalf("%s was accepted: %s", name, out)
		}
	}
	if _, err := (Adapter{}).DecodeState(bus.StateInput{State: json.RawMessage(`{"schema":2}`)}); bus.ErrorCodeOf(err) != bus.CodeUnsupportedSchema {
		t.Fatal("a future Cursor schema was not reported as unsupported", err)
	}
}
