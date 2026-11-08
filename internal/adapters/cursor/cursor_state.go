package cursor

import (
	"encoding/json"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// listenerState is Cursor's persisted recovery state.
// The listener endpoint is core's listener lease.
type listenerState struct {
	extensions         map[string]json.RawMessage
	Schema             int  `json:"schema"`
	RecoveryAttempted  bool `json:"recovery_attempted,omitempty"`
	RecoverySuppressed bool `json:"recovery_suppressed,omitempty"`
}

func decodeCursorState(raw json.RawMessage) listenerState {
	var state listenerState
	_ = json.Unmarshal(raw, &state) // Core validates through DecodeState on journal read.
	_ = json.Unmarshal(raw, &state.extensions)
	for key := range state.extensions {
		for _, known := range []string{"schema", "recovery_attempted", "recovery_suppressed"} {
			if strings.EqualFold(key, known) {
				delete(state.extensions, key)
				break
			}
		}
	}
	return state
}

func encodeCursorState(state listenerState) json.RawMessage {
	state.Schema = 1
	raw, _ := json.Marshal(state) // A struct of plain fields always marshals.
	if len(state.extensions) == 0 {
		return raw
	}
	fields := map[string]json.RawMessage{}
	for key, value := range state.extensions {
		fields[key] = value
	}
	known := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &known)
	for key, value := range known {
		fields[key] = value
	}
	raw, _ = json.Marshal(fields)
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
	if state.Schema != 1 {
		return nil, &model.Error{Code: model.CodeUnsupportedSchema, Message: "unsupported Cursor state schema"}
	}
	return encodeCursorState(decodeCursorState(in.State)), nil
}
