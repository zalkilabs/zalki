package agent

import (
	"context"

	"github.com/zalkilabs/zalki/internal/model"
)

// Lifecycle states how an adapter's client behaves, so core never switches on
// an event name for one client. The zero value is the default: a session end
// retires the session, and a prompt hook cannot register an unknown session.
type Lifecycle struct {
	// KeepOnSessionEnd keeps the session listed and ready on SessionEnd. Use it
	// when the client also ends sessions that are only unloaded, not closed.
	KeepOnSessionEnd bool
	// PromptMayRegister lets a trusted prompt hook register a session core has
	// not seen yet. Only core sets this on an event; JSON input cannot.
	PromptMayRegister bool
}

type AdapterDescriptor struct {
	Kind, Label  string
	Hooks        map[string]string // Native event name to the common lifecycle event.
	Lifecycle    Lifecycle
	ProcessNames []string
	OwnerRole    string
	Executable   string
	Notes        []string
}

type Adapter interface {
	Descriptor() AdapterDescriptor
	ReceiveCapabilities(model.Session) model.ReceiveCapabilities
	Deliver(context.Context, model.Message, model.Session) model.Receipt
}
