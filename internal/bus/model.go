package bus

import "github.com/zalkilabs/zalki/internal/model"

// Endpoint is the compatibility name for model.Endpoint.
type Endpoint = model.Endpoint

// Session is the compatibility name for model.Session.
type Session = model.Session

// ListenerLease identifies the one armed listener of a registration. Only the
// process holding the matching in-memory Lease may confirm, validate or clear it.
type ListenerLease = model.ListenerLease

// ReceiveCapabilities is the compatibility name for model.ReceiveCapabilities.
type ReceiveCapabilities = model.ReceiveCapabilities

// Message is the compatibility name for model.Message.
type Message = model.Message
