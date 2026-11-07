package cursor

import (
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestCursorStateMigratesLegacyFieldsAndDropsPreLeaseWake(t *testing.T) {
	out, err := (Adapter{}).DecodeState(agent.StateInput{Extensions: map[string]json.RawMessage{
		"cursor_recovery_attempted":  json.RawMessage(`true`),
		"cursor_recovery_suppressed": json.RawMessage(`true`),
		"cursor_wake":                json.RawMessage(`{"path":"/tmp/zalki-old/inbox.sock"}`),
	}})
	if err != nil || string(out) != `{"schema":1,"recovery_attempted":true,"recovery_suppressed":true}` {
		t.Fatalf("legacy fields were not migrated exactly: %s %v", out, err)
	}
	out, err = (Adapter{}).DecodeState(agent.StateInput{State: json.RawMessage(`{"schema":1,"recovery_attempted":true,"wake":{"path":"/tmp/zalki-old/inbox.sock"}}`)})
	if err != nil || string(out) != `{"schema":1,"recovery_attempted":true}` {
		t.Fatalf("a pre-lease wake survived normalization: %s %v", out, err)
	}
	if out, err := (Adapter{}).DecodeState(agent.StateInput{}); err != nil || string(out) != `{"schema":1}` {
		t.Fatalf("a record without state did not get the current schema: %s %v", out, err)
	}
}

func TestCursorStateRejectsUnknownSchemasAndMalformedLegacyFields(t *testing.T) {
	for name, in := range map[string]agent.StateInput{
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
	if _, err := (Adapter{}).DecodeState(agent.StateInput{State: json.RawMessage(`{"schema":2}`)}); bus.ErrorCodeOf(err) != model.CodeUnsupportedSchema {
		t.Fatal("a future Cursor schema was not reported as unsupported", err)
	}
}

func TestCursorStateKeepsUnknownFieldsWhenKnownFlagsAreCleared(t *testing.T) {
	raw := json.RawMessage(`{"schema":1,"recovery_attempted":true,"recovery_suppressed":true,"wake":{"path":"/old"},"future_flag":{"large":123456789012345678901234567890}}`)
	normalized, err := (Adapter{}).DecodeState(agent.StateInput{State: raw})
	if err != nil {
		t.Fatal(err)
	}
	state := decodeCursorState(normalized)
	state.RecoveryAttempted, state.RecoverySuppressed = false, false
	encoded := encodeCursorState(state)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["future_flag"]) != `{"large":123456789012345678901234567890}` {
		t.Fatal("extension lost", string(encoded))
	}
	for _, key := range []string{"recovery_attempted", "recovery_suppressed", "wake"} {
		if _, exists := fields[key]; exists {
			t.Fatal("cleared or obsolete field resurrected", key)
		}
	}
}
