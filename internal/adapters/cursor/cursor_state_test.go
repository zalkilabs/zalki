package cursor

import (
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestCursorStateStartsEmpty(t *testing.T) {
	if out, err := (Adapter{}).DecodeState(agent.StateInput{}); err != nil || string(out) != `{}` {
		t.Fatalf("a record without state = %s %v, want {}", out, err)
	}
}

// The two recovery flags round-trip; any other key is dropped.
func TestCursorStateKeepsOnlyTheRecoveryFlags(t *testing.T) {
	raw := json.RawMessage(`{"schema":1,"recovery_attempted":true,"recovery_suppressed":true,"wake":{"path":"/old"}}`)
	out, err := (Adapter{}).DecodeState(agent.StateInput{State: raw})
	if err != nil || string(out) != `{"recovery_attempted":true,"recovery_suppressed":true}` {
		t.Fatalf("state = %s %v", out, err)
	}
	state := decodeCursorState(out)
	state.RecoveryAttempted, state.RecoverySuppressed = false, false
	if encoded := encodeCursorState(state); string(encoded) != `{}` {
		t.Fatalf("cleared flags = %s, want {}", encoded)
	}
}

func TestCursorStateRejectsMalformedState(t *testing.T) {
	if out, err := (Adapter{}).DecodeState(agent.StateInput{State: json.RawMessage(`{"recovery_attempted":`)}); err == nil {
		t.Fatalf("malformed state was accepted: %s", out)
	}
}
