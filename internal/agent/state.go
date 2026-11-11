package agent

import "encoding/json"

// StateInput is what an adapter's state codec may read. It never carries the
// session: State is a copy of the adapter's own persisted state. Credentials,
// identity and every other core field are removed before the codec runs.
type StateInput struct {
	State json.RawMessage
}

// SessionStateAdapter initializes or validates an adapter's persisted state. It
// returns the normalized current-schema state; core bounds, copies and stores
// only that result. An error leaves the record unread or unchanged.
type SessionStateAdapter interface {
	DecodeState(StateInput) (json.RawMessage, error)
}
