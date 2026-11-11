package model

import (
	"errors"
	"strings"
	"time"
)

// Event is the minimal metadata event used by the commit mechanism.
// It deliberately has no body, body size, credential, label, path or adapter
// state. A session event sets Session; a message event sets Message.
type Event struct {
	// ID identifies this immutable event across recovery and replay.
	ID string `json:"id"`
	// Type identifies the supported state-change fact.
	Type string `json:"type"`
	// OccurredAt is the captured UTC time, not a global ordering key.
	OccurredAt string `json:"occurred_at"`
	// TransactionID groups the event with its committed after-image.
	TransactionID string `json:"transaction_id"`
	// Session contains only the affected session's canonical metadata.
	Session EventSession `json:"session,omitzero"`
	// Message contains only the affected message's delivery metadata.
	Message *EventMessage `json:"message,omitempty"`
}

// Event types.
const (
	EventSessionRegistered    = "session.registered"
	EventSessionStateChanged  = "session.state_changed"
	EventMessageStatusChanged = "message.status_changed"
)

// EventCause names why a session state changed. Empty means no specific cause.
type EventCause string

// Session state change causes.
const (
	CauseRegister    EventCause = "register"
	CauseStop        EventCause = "stop"
	CauseInterrupt   EventCause = "interrupt"
	CausePrompt      EventCause = "prompt"
	CauseTool        EventCause = "tool"
	CausePermission  EventCause = "permission"
	CauseSessionEnd  EventCause = "session_end"
	CauseOwnerExited EventCause = "owner_exited"
	CauseCleanup     EventCause = "cleanup"
	CauseRecovery    EventCause = "recovery"
)

// Valid reports whether c is empty or a known cause.
func (c EventCause) Valid() bool {
	switch c {
	case "", CauseRegister, CauseStop, CauseInterrupt, CausePrompt, CauseTool, CausePermission,
		CauseSessionEnd, CauseOwnerExited, CauseCleanup, CauseRecovery:
		return true
	}
	return false
}

// EventSession describes a persisted session state change, not task completion.
type EventSession struct {
	// ID is the canonical kind:UUID address.
	ID string `json:"id"`
	// Generation identifies the receiving registration.
	Generation string `json:"generation"`
	// Previous is the persisted state before this change.
	Previous SessionState `json:"previous,omitempty"`
	// State is the resulting persisted state.
	State SessionState `json:"state"`
	// PreviousRetired and Retired preserve retirement facts independently of state.
	PreviousRetired bool `json:"previous_retired,omitempty"`
	Retired         bool `json:"retired,omitempty"`
	// Cause is why the state changed, when the writer knows it.
	Cause EventCause `json:"cause,omitempty"`
}

// EventMessage describes a change of a message's status, hold or
// acknowledgement. An empty Previous means the message was created.
type EventMessage struct {
	ID      string `json:"id"`
	ReplyTo string `json:"reply_to,omitempty"`
	// From and To are canonical kind:UUID addresses in the journal. A plugin
	// sees "kind:" for a party that is not one of its feed sessions.
	From                 string         `json:"from"`
	To                   string         `json:"to"`
	Kind                 string         `json:"kind,omitempty"`
	Previous             DeliveryStatus `json:"previous,omitempty"`
	Status               DeliveryStatus `json:"status"`
	PreviousHeld         bool           `json:"previous_held,omitempty"`
	Held                 bool           `json:"held,omitempty"`
	PreviousAcknowledged bool           `json:"previous_acknowledged,omitempty"`
	Acknowledged         bool           `json:"acknowledged,omitempty"`
}

// Validate checks a stored event without performing IO.
func (e Event) Validate() error {
	for _, id := range []string{e.ID, e.TransactionID} {
		if !IsCanonicalUUID(id) {
			return errors.New("invalid event identity")
		}
	}
	switch e.Type {
	case EventSessionRegistered, EventSessionStateChanged:
		if e.Message != nil || !IsCanonicalUUID(e.Session.Generation) {
			return errors.New("invalid event identity")
		}
		if err := e.Session.validate(e.Type); err != nil {
			return err
		}
	case EventMessageStatusChanged:
		if e.Message == nil || e.Session != (EventSession{}) {
			return errors.New("invalid event identity")
		}
		if err := e.Message.validate(); err != nil {
			return err
		}
	default:
		return errors.New("unsupported event type")
	}
	stamp, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
	if err != nil || stamp.IsZero() || !strings.HasSuffix(e.OccurredAt, "Z") {
		return errors.New("event requires a UTC timestamp")
	}
	return nil
}

func (e EventSession) validate(kindOfEvent string) error {
	kind, id, ok := strings.Cut(e.ID, ":")
	canonical, err := UUID(id)
	if !ok || !ValidKind(kind) || err != nil || canonical != id {
		return errors.New("invalid event session identity")
	}
	if !e.State.Valid() || (e.Previous != "" && !e.Previous.Valid()) {
		return errors.New("invalid event session state")
	}
	if kindOfEvent == EventSessionStateChanged && (!e.Previous.Valid() || (e.Previous == e.State && e.PreviousRetired == e.Retired)) {
		return errors.New("event requires a session state change")
	}
	if !e.Cause.Valid() {
		return errors.New("invalid event session cause")
	}
	return nil
}

func (m EventMessage) validate() error {
	if !m.validIdentity() {
		return errors.New("invalid event message identity")
	}
	if !m.validStatus() {
		return errors.New("invalid event message status")
	}
	if m.Previous != "" && !m.changed() {
		return errors.New("event requires a message change")
	}
	if m.Previous == "" && (m.PreviousHeld || m.PreviousAcknowledged) {
		return errors.New("invalid event message status")
	}
	return nil
}

// validIdentity checks the message, reply and party addresses.
func (m EventMessage) validIdentity() bool {
	return IsCanonicalUUID(m.ID) && (m.ReplyTo == "" || IsCanonicalUUID(m.ReplyTo)) && eventAddress(m.From) && eventAddress(m.To)
}

// validStatus checks both statuses and the message kind.
func (m EventMessage) validStatus() bool {
	kind := m.Kind == "" || (len(m.Kind) <= 64 && ValidKind(m.Kind))
	return m.Status.Valid() && (m.Previous == "" || m.Previous.Valid()) && kind
}

// changed reports whether the status, hold or acknowledgement changed.
func (m EventMessage) changed() bool {
	return m.Previous != m.Status || m.PreviousHeld != m.Held || m.PreviousAcknowledged != m.Acknowledged
}

// eventAddress accepts a canonical kind:UUID address, which includes plugin
// inbox addresses.
func eventAddress(address string) bool {
	kind, id, ok := strings.Cut(address, ":")
	return ok && ValidKind(kind) && IsCanonicalUUID(id)
}
