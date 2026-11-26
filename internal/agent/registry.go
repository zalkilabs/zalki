package agent

import (
	"errors"
	"fmt"
	"sort"

	"github.com/zalkilabs/zalki/internal/model"
)

type Registry struct{ adapters map[string]Adapter }

func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{adapters: make(map[string]Adapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil {
			return nil, errors.New("nil agent adapter")
		}
		d := adapter.Descriptor()
		if !model.ValidKind(d.Kind) || !model.ValidName(d.Label) {
			return nil, errors.New("invalid agent kind or display label")
		}
		if _, exists := r.adapters[d.Kind]; exists {
			return nil, fmt.Errorf("duplicate agent adapter %q", d.Kind)
		}
		if err := validateAdapterCapabilities(adapter, d); err != nil {
			return nil, err
		}
		r.adapters[d.Kind] = adapter
	}
	return r, nil
}

func validateAdapterCapabilities(adapter Adapter, d AdapterDescriptor) error {
	capabilities := adapter.ReceiveCapabilities(model.Session{})
	if err := capabilities.Validate(); err != nil {
		return fmt.Errorf("%s capabilities: %w", d.Kind, err)
	}
	if capabilities.LifecycleHooks != (len(d.Hooks) > 0) {
		return fmt.Errorf("%s hook capabilities do not match its descriptor", d.Kind)
	}
	for native, common := range d.Hooks {
		if !model.ValidName(native) || StandardEvents(true)[common] == "" {
			return fmt.Errorf("%s declares an unsupported lifecycle event", d.Kind)
		}
	}
	if len(d.Hooks) > 0 {
		if _, ok := adapter.(HookAdapter); !ok {
			return fmt.Errorf("%s has no lifecycle adapter", d.Kind)
		}
	}
	return validateHookEventKey(d)
}

// validateHookEventKey checks a declared hook event key. Core reads the event
// name under this key, so it must be a plain name and the adapter must have
// hooks. That every hook adapter declares a key is checked by agenttest.
func validateHookEventKey(d AdapterDescriptor) error {
	if d.HookEventKey == "" {
		return nil
	}
	if !model.ValidName(d.HookEventKey) {
		return fmt.Errorf("%s declares an invalid hook event key", d.Kind)
	}
	if len(d.Hooks) == 0 {
		return fmt.Errorf("%s declares a hook event key but no hooks", d.Kind)
	}
	return nil
}

var errNoRegistry = model.NewError(model.CodeInternal, "agent registry is not configured")

func (r *Registry) Lookup(kind string) (Adapter, error) {
	if r == nil {
		return nil, errNoRegistry
	}
	adapter, ok := r.adapters[kind]
	if !ok {
		return nil, model.NewError(model.CodeInvalidInput, "unsupported agent %q", kind)
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
