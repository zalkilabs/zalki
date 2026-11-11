package cursor

import (
	"encoding/json"

	"github.com/zalkilabs/zalki/internal/agent"
)

// listenerState is Cursor's persisted recovery state.
// The listener endpoint is core's listener lease.
type listenerState struct {
	RecoveryAttempted  bool `json:"recovery_attempted,omitempty"`
	RecoverySuppressed bool `json:"recovery_suppressed,omitempty"`
}

// decodeCursorState reads state that DecodeState already accepted on the
// journal read. A decode error is ignored and may leave some fields decoded.
func decodeCursorState(raw json.RawMessage) listenerState {
	var state listenerState
	_ = json.Unmarshal(raw, &state)
	return state
}

func encodeCursorState(state listenerState) json.RawMessage {
	raw, _ := json.Marshal(state) // A struct of plain fields always marshals.
	return raw
}

// DecodeState initializes or validates Cursor's current state in memory.
// Core persists the result only within a journal operation.
func (Adapter) DecodeState(in agent.StateInput) (json.RawMessage, error) {
	if len(in.State) == 0 {
		return encodeCursorState(listenerState{}), nil
	}
	var state listenerState
	if err := json.Unmarshal(in.State, &state); err != nil {
		return nil, err
	}
	return encodeCursorState(state), nil
}
