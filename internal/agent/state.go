package agent

import "encoding/json"

// StateInput is what an adapter's state codec may read. It never carries the
// session: State is a copy of the adapter's own persisted state, and
// Extensions holds a copy of each top-level record field core does not own,
// such as a client's fields from before adapter state existed. Credentials,
// identity and every other core field are removed before the codec runs.
type StateInput struct {
	Extensions map[string]json.RawMessage
	State      json.RawMessage
}

// SessionStateAdapter validates or migrates an adapter's persisted state. It
// returns the normalized current-schema state; core bounds, copies and stores
// only that result. An error leaves the record unread or unchanged.
type SessionStateAdapter interface {
	DecodeState(StateInput) (json.RawMessage, error)
}

// SessionStateMigrationAdapter identifies obsolete top-level extension fields
// consumed by an adapter's migration. Core-owned fields can never be removed.
type SessionStateMigrationAdapter interface {
	LegacyStateFields() []string
}
