package cursor

import (
	"encoding/json"

	"github.com/zalkilabs/zalki/internal/bus"
)

// listenerState is Cursor's persisted recovery state. Its schema and legacy
// migration stay together. The listener endpoint is core's listener lease.
type listenerState struct {
	Schema             int  `json:"schema"`
	RecoveryAttempted  bool `json:"recovery_attempted,omitempty"`
	RecoverySuppressed bool `json:"recovery_suppressed,omitempty"`
}

func decodeCursorState(raw json.RawMessage) listenerState {
	var state listenerState
	_ = json.Unmarshal(raw, &state) // Core validates through DecodeState on journal read.
	return state
}

func encodeCursorState(state listenerState) json.RawMessage {
	state.Schema = 1
	raw, _ := json.Marshal(state) // A struct of plain fields always marshals.
	return raw
}

// DecodeState validates or migrates Cursor's state in memory; core persists the
// result only within a journal operation.
//
// Records from before adapter state kept Cursor's recovery flags as top-level
// fields. Wake endpoints recorded by builds before listener leases are dropped,
// not adopted: they prove no lease, and under the upgrade policy their
// listeners are restarted. Re-encoding also keeps a later write from carrying
// a legacy endpoint into a record that older builds can read.
func (Adapter) DecodeState(in bus.StateInput) (json.RawMessage, error) {
	if len(in.State) == 0 {
		var legacy listenerState
		for field, target := range map[string]*bool{
			"cursor_recovery_attempted":  &legacy.RecoveryAttempted,
			"cursor_recovery_suppressed": &legacy.RecoverySuppressed,
		} {
			if value, ok := in.Extensions[field]; ok {
				if err := json.Unmarshal(value, target); err != nil {
					return nil, err
				}
			}
		}
		return encodeCursorState(legacy), nil
	}
	var state listenerState
	if err := json.Unmarshal(in.State, &state); err != nil {
		return nil, err
	}
	if state.Schema != 1 {
		return nil, &bus.Error{Code: bus.CodeUnsupportedSchema, Message: "unsupported Cursor state schema"}
	}
	return encodeCursorState(state), nil
}
