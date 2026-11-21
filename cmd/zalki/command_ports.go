package main

import (
	"context"
	"io"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// Each command family sees only the bus calls it makes.

// hookRunner is what the hook command needs.
type hookRunner interface {
	Hook(ctx context.Context, kind string, input io.Reader, output io.Writer) error
}

// sessionPort is what the send and cursor-listen commands need.
type sessionPort interface {
	Send(ctx context.Context, request bus.SendRequest) (model.Message, error)
	TrackRuntime(role, actor string) (*bus.RuntimeLease, error)
	AcquireListenerLease(actor, key string) (*bus.Lease, error)
	Registry() *agent.Registry
}

// diagnosticsPort is what the list, doctor and prune commands need.
type diagnosticsPort interface {
	ListWithWarnings() ([]model.Session, int, error)
	Diagnostics(ctx context.Context) (bus.DiagnosticReport, error)
	PruneRetired(dryRun bool, selected []string, retiredOlderThan time.Duration) (bus.PruneReport, error)
	NoticeExitedSessions(ctx context.Context) error
	Dir() string
}

// serviceView meets the ports without declaring a method: the store calls
// come from the embedded store, Send, Hook and the notices from the service.
type serviceView struct {
	bus.Store
	*bus.Service
}

func viewOf(service *bus.Service) serviceView {
	return serviceView{Store: service.Store, Service: service}
}

var (
	_ hookRunner      = serviceView{}
	_ sessionPort     = serviceView{}
	_ diagnosticsPort = serviceView{}
)
