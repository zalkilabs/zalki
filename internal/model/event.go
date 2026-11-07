package model

import (
	"errors"
	"strings"
	"time"
)

// Event is the minimal metadata event used by the dormant commit mechanism.
// It deliberately has no body, credential, label, path or adapter state.
type Event struct {
	// Schema is zero while this metadata contract is experimental.
	Schema int `json:"schema"`
	// ID identifies this immutable event across recovery and replay.
	ID string `json:"id"`
	// Type identifies the supported state-change fact.
	Type string `json:"type"`
	// OccurredAt is the captured UTC time, not a global ordering key.
	OccurredAt string `json:"occurred_at"`
	// TransactionID groups the event with its committed after-image.
	TransactionID string `json:"transaction_id"`
	// Session contains only the affected session's canonical metadata.
	Session EventSession `json:"session"`
}

// EventSession describes a persisted session state change, not task completion.
type EventSession struct {
	// ID is the canonical kind:UUID address.
	ID string `json:"id"`
	// Generation identifies the receiving registration.
	Generation string `json:"generation"`
	// Previous is the persisted state before this change.
	Previous SessionState `json:"previous"`
	// State is the resulting persisted state.
	State SessionState `json:"state"`
}

// Validate checks the experimental state-change event without performing IO.
func (e Event) Validate() error {
	if e.Schema != 0 || e.Type != "session.state_changed" {
		return errors.New("unsupported event schema or type")
	}
	for _, id := range []string{e.ID, e.TransactionID, e.Session.Generation} {
		if canonical, err := UUID(id); err != nil || canonical != id {
			return errors.New("invalid event identity")
		}
	}
	if err := e.Session.validate(); err != nil {
		return err
	}
	stamp, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
	if err != nil || stamp.IsZero() || !strings.HasSuffix(e.OccurredAt, "Z") {
		return errors.New("event requires a UTC timestamp")
	}
	return nil
}

func (e EventSession) validate() error {
	kind, id, ok := strings.Cut(e.ID, ":")
	canonical, err := UUID(id)
	if !ok || !ValidKind(kind) || err != nil || canonical != id {
		return errors.New("invalid event session identity")
	}
	if !e.Previous.Valid() || !e.State.Valid() || e.Previous == e.State {
		return errors.New("event requires a session state change")
	}
	return nil
}
