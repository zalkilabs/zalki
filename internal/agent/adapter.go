package agent

import (
	"context"

	"github.com/zalkilabs/zalki/internal/model"
)

type AdapterDescriptor struct {
	Kind, Label  string
	Hooks        map[string]string // Native event name to the common lifecycle event.
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
