package bus

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// InboxMode is the compatibility name for model.InboxMode.
type InboxMode = model.InboxMode

// DeliveryTiming is the compatibility name for model.DeliveryTiming.
type DeliveryTiming = model.DeliveryTiming

const (
	InboxNative          = model.InboxNative
	InboxJournal         = model.InboxJournal
	DeliveryNextTurn     = model.DeliveryNextTurn
	DeliveryBetweenTools = model.DeliveryBetweenTools
	DeliveryNextHook     = model.DeliveryNextHook
	DeliveryNotification = model.DeliveryNotification
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
	ReceiveCapabilities(Session) ReceiveCapabilities
	Deliver(context.Context, Message, Session) Receipt
}

// NameAdapter supplies native session labels and their configuration location.
type NameAdapter interface {
	ConfigDir() string
	LookupName(Session, map[string]map[string]string) (string, string)
}

// HealthAdapter supplements common diagnostics with transport and listener health.
type HealthAdapter interface {
	Health(HealthHost, Session) (transport, listener string)
}

// RecoveryHintAdapter supplies a client-specific next step after core's common
// hints (retirement, exited owner, outdated build) found nothing to suggest.
type RecoveryHintAdapter interface {
	RecoveryHint(SessionDiagnostic) string
}

// CompatibilityAdapter supplements doctor with adapter-owned version evidence.
// Empty or malformed results leave the common fixture diagnosis unchanged.
type CompatibilityAdapter interface {
	Compatibility(version string) (status, hint string)
}

type Registry struct{ adapters map[string]Adapter }

// validKind forwards syntax validation without consulting the registry.
func validKind(kind string) bool { return model.ValidKind(kind) }

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil agent adapter")
		}
		d := adapter.Descriptor()
		if !validKind(d.Kind) || !validName(d.Label) {
			return nil, errors.New("invalid agent kind or display label")
		}
		if _, exists := r.adapters[d.Kind]; exists {
			return nil, fmt.Errorf("duplicate agent adapter %q", d.Kind)
		}
		capabilities := adapter.ReceiveCapabilities(Session{})
		if err := capabilities.Validate(); err != nil {
			return nil, fmt.Errorf("%s capabilities: %w", d.Kind, err)
		}
		if capabilities.LifecycleHooks != (len(d.Hooks) > 0) {
			return nil, fmt.Errorf("%s hook capabilities do not match its descriptor", d.Kind)
		}
		for native, common := range d.Hooks {
			if !validName(native) || agent.StandardEvents(true)[common] == "" {
				return nil, fmt.Errorf("%s declares an unsupported lifecycle event", d.Kind)
			}
		}
		if len(d.Hooks) > 0 {
			if _, ok := adapter.(agent.HookAdapter); !ok {
				return nil, fmt.Errorf("%s has no lifecycle adapter", d.Kind)
			}
		}
		r.adapters[d.Kind] = adapter
	}
	return r, nil
}

var errNoRegistry = fail(CodeInternal, "agent registry is not configured")

func (r *Registry) Lookup(kind string) (Adapter, error) {
	if r == nil {
		return nil, errNoRegistry
	}
	adapter, ok := r.adapters[kind]
	if !ok {
		return nil, fail(CodeInvalidInput, "unsupported agent %q", kind)
	}
	return adapter, nil
}

func (r *Registry) Kinds() []string {
	if r == nil {
		return nil
	}
	kinds := make([]string, 0, len(r.adapters))
	for kind := range r.adapters {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

func (s Store) registry() *Registry { return s.Registry }
