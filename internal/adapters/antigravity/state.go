package antigravity

import (
	"encoding/json"

	"github.com/zalkilabs/zalki/internal/agent"
)

// maxContinues is how many times in a row a Stop hook may continue a turn with
// message text. The next Stop that would be the sixth ends the turn instead.
const maxContinues = 5

// loopState is Antigravity's persisted state: how many turn continuations in a
// row delivered messages. Antigravity's own continuationActive flag did not turn
// true after a continue in our runs, so Zalki counts itself.
type loopState struct {
	Continues int `json:"continues,omitempty"`
}

// decodeState reads state that DecodeState already accepted on the journal
// read. A decode error is ignored and may leave some fields decoded.
func decodeState(raw json.RawMessage) loopState {
	var state loopState
	_ = json.Unmarshal(raw, &state)
	return state
}

func encodeState(state loopState) json.RawMessage {
	raw, _ := json.Marshal(state) // A struct of plain fields always marshals.
	return raw
}

// DecodeState initializes or validates the adapter's current state in memory.
// Core persists the result only within a journal operation.
func (Adapter) DecodeState(in agent.StateInput) (json.RawMessage, error) {
	if len(in.State) == 0 {
		return encodeState(loopState{}), nil
	}
	var state loopState
	if err := json.Unmarshal(in.State, &state); err != nil {
		return nil, err
	}
	if state.Continues < 0 || state.Continues > maxContinues {
		state.Continues = 0
	}
	return encodeState(state), nil
}
