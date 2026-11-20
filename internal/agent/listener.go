package agent

import (
	"context"
	"encoding/json"
	"io"

	"github.com/zalkilabs/zalki/internal/model"
)

// ListenerHandle is a listener's view of a core-owned lease. A listener uses
// it from one goroutine; core retains authentication, fencing and journal IO.
type ListenerHandle interface {
	// Generation identifies the registration for which this handle was acquired.
	Generation() string
	// Arm binds the handle to the inspected endpoint.
	Arm(*model.Endpoint) error
	// Confirm applies adapter state after readiness was announced. Core runs the
	// policy inside its journal transaction, so the policy must be fast and pure.
	Confirm(func(model.SessionView) (json.RawMessage, error)) error
	// ValidateDelivery checks recipient, generation, pending state and prior ACK.
	ValidateDelivery(messageID string) (acknowledged bool, err error)
	// RejectHint records a rejected notification without exposing its contents.
	RejectHint(error)
	// WakeEmitted records output written for a validated message.
	WakeEmitted(messageID string)
	// Close releases this handle's lease and lifetime lock. It is idempotent.
	Close(cause error)
}

// Listener is an optional adapter capability: a long-running native listener
// that wakes an idle session. The caller supplies lease acquisition; the
// adapter owns its socket and never changes the lease lifecycle.
type Listener interface {
	Listen(ctx context.Context, acquire func(actor, key string) (ListenerHandle, error), actor, key string, output io.Writer) error
}
