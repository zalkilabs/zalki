package bus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
)

type InboxMode string
type DeliveryTiming string

const (
	InboxNative          InboxMode      = "native"
	InboxJournal         InboxMode      = "journal"
	DeliveryNextTurn     DeliveryTiming = "next_turn"
	DeliveryBetweenTools DeliveryTiming = "between_tools"
	DeliveryNextHook     DeliveryTiming = "next_hook"
	DeliveryNotification DeliveryTiming = "native_notification"
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

type nameAdapter interface {
	ConfigDir() string
	LookupName(Session, map[string]map[string]string) (string, string)
}

type StartupContext struct {
	Actor, Capability, StateDir, Instructions string
}

// HookRegistration is what an adapter reports when a lifecycle hook registers a session.
type HookRegistration struct {
	Endpoint           *Endpoint
	PreserveGeneration bool
}

type hookAdapter interface {
	NormalizeHook(HookInput) (HookInput, error)
	RegisterHook(HookInput) (HookRegistration, error)
	HandleHook(HookHost, HookInput, string, io.Writer) (bool, error)
	StartupOutput(StartupContext) (any, error)
}

type healthAdapter interface {
	Health(HealthHost, Session) (transport, listener string)
}

type Registry struct{ adapters map[string]Adapter }

var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil agent adapter")
		}
		d := adapter.Descriptor()
		if !kindPattern.MatchString(d.Kind) || !validName(d.Label) {
			return nil, errors.New("invalid agent kind or display label")
		}
		if _, exists := r.adapters[d.Kind]; exists {
			return nil, fmt.Errorf("duplicate agent adapter %q", d.Kind)
		}
		capabilities := adapter.ReceiveCapabilities(Session{})
		if err := capabilities.validate(); err != nil {
			return nil, fmt.Errorf("%s capabilities: %w", d.Kind, err)
		}
		if capabilities.LifecycleHooks != (len(d.Hooks) > 0) {
			return nil, fmt.Errorf("%s hook capabilities do not match its descriptor", d.Kind)
		}
		for native, common := range d.Hooks {
			if !validName(native) || StandardEvents(true)[common] == "" {
				return nil, fmt.Errorf("%s declares an unsupported lifecycle event", d.Kind)
			}
		}
		if len(d.Hooks) > 0 {
			if _, ok := adapter.(hookAdapter); !ok {
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

func (c ReceiveCapabilities) validate() error {
	if c.Delivery == "" || (c.Inbox != InboxNative && c.Inbox != InboxJournal) {
		return errors.New("delivery and inbox mode must be declared")
	}
	if c.HookDelivery && c.Inbox != InboxJournal {
		return errors.New("hook delivery requires a journal inbox")
	}
	switch c.BusyDelivery {
	case DeliveryNextTurn, DeliveryBetweenTools, DeliveryNextHook, DeliveryNotification:
	default:
		return errors.New("busy delivery timing must be declared")
	}
	return nil
}

// StandardHooks supplies default lifecycle behavior for adapters that need none of their own.
type StandardHooks struct{}

func (StandardHooks) NormalizeHook(event HookInput) (HookInput, error) { return event, nil }
func (StandardHooks) RegisterHook(HookInput) (HookRegistration, error) {
	return HookRegistration{}, nil
}
func (StandardHooks) HandleHook(HookHost, HookInput, string, io.Writer) (bool, error) {
	return false, nil
}
func (StandardHooks) StartupOutput(startup StartupContext) (any, error) {
	return map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "SessionStart", "additionalContext": startup.Instructions}}, nil
}

// StandardEvents lists the common lifecycle events adapters may map native hook names onto.
func StandardEvents(interrupt bool) map[string]string {
	events := map[string]string{}
	for _, event := range []string{"SessionStart", "SessionEnd", "PermissionRequest", "PostToolUse", "UserPromptSubmit", "Stop"} {
		events[event] = event
	}
	if interrupt {
		events["Interrupt"] = "Interrupt"
	}
	return events
}
