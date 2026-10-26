package bus

import "encoding/json"

// CursorListenerState is the persisted Cursor runtime contract shared by the
// adapter and atomic listener transactions. Its schema and legacy migration stay
// together; changing this in-memory value never writes or authenticates a journal.
type CursorListenerState struct {
	Schema             int       `json:"schema"`
	Wake               *Endpoint `json:"wake,omitempty"`
	RecoveryAttempted  bool      `json:"recovery_attempted,omitempty"`
	RecoverySuppressed bool      `json:"recovery_suppressed,omitempty"`
}

func cursorState(v Session) CursorListenerState {
	var state CursorListenerState
	_ = json.Unmarshal(v.AdapterState, &state) // Validated on journal read.
	return state
}
func setCursorState(v *Session, state CursorListenerState) {
	state.Schema = 1
	v.AdapterState, _ = json.Marshal(state)
}

// Restore validates or migrates a session's listener state in memory. It never
// changes a record on disk; the core persists it only within a journal operation.
func (CursorListenerState) Restore(v *Session, raw json.RawMessage) error {
	if len(v.AdapterState) == 0 {
		var legacy struct {
			Wake       *Endpoint `json:"cursor_wake"`
			Attempted  bool      `json:"cursor_recovery_attempted"`
			Suppressed bool      `json:"cursor_recovery_suppressed"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return err
		}
		setCursorState(v, CursorListenerState{Wake: legacy.Wake, RecoveryAttempted: legacy.Attempted, RecoverySuppressed: legacy.Suppressed})
		return nil
	}
	var state CursorListenerState
	if err := json.Unmarshal(v.AdapterState, &state); err != nil {
		return err
	}
	if state.Schema != 1 {
		return fail(CodeUnsupportedSchema, "unsupported Cursor state schema %d", state.Schema)
	}
	return nil
}
