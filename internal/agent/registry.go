package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

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
		if err := errors.Join(declarationProblems(adapter, d)...); err != nil {
			return nil, err
		}
		r.adapters[d.Kind] = adapter
	}
	return r, nil
}

// declarationProblems lists everything wrong with one adapter's declaration, in
// a fixed order, so that one run shows all of it. Every rule here is decided from
// data the registry already reads: the descriptor, the optional interfaces the
// adapter implements, and its capabilities for an empty session. Rules that need
// to call the adapter's hook or state code are behavior checks and live in
// agenttest. The first rules keep their order: callers and tests rely on the first line.
func declarationProblems(adapter Adapter, d AdapterDescriptor) []error {
	var problems []error
	add := func(err error) {
		if err != nil {
			problems = append(problems, err)
		}
	}
	capabilities := adapter.ReceiveCapabilities(model.Session{})
	if err := capabilities.Validate(); err != nil {
		add(fmt.Errorf("%s capabilities: %w", d.Kind, err))
	}
	if capabilities.LifecycleHooks != (len(d.Hooks) > 0) {
		add(fmt.Errorf("%s hook capabilities do not match its descriptor", d.Kind))
	}
	for native, common := range d.Hooks {
		if !model.ValidName(native) || !commonEvent(common) {
			add(fmt.Errorf("%s declares an unsupported lifecycle event", d.Kind))
			break
		}
	}
	_, hooked := adapter.(HookAdapter)
	if len(d.Hooks) > 0 && !hooked {
		add(fmt.Errorf("%s has no lifecycle adapter", d.Kind))
	}
	add(validateHookDeclaration(d, hooked))
	add(validateHookEventKey(d))
	add(validateNativeDeclaration(adapter, d, hooked))
	add(validateUncertainWording(d))
	add(validateInstallNotice(d))
	add(validatePromptReply(d))
	add(validateActivityRule(d))
	return problems
}

// validatePromptReply checks the reply core writes after a prompt hook that may
// register a session: one JSON object on one line, and only with PromptMayRegister.
func validatePromptReply(d AdapterDescriptor) error {
	reply := d.Lifecycle.PromptReply
	if reply == "" {
		return nil
	}
	if !d.Lifecycle.PromptMayRegister {
		return fmt.Errorf("%s declares a PromptReply but not PromptMayRegister; core writes the reply only after a prompt hook that may register a session", d.Kind)
	}
	var object map[string]json.RawMessage
	if strings.ContainsAny(reply, "\r\n") || json.Unmarshal([]byte(reply), &object) != nil || object == nil {
		return fmt.Errorf("%s: PromptReply must be one JSON object on one line without a newline, got %q", d.Kind, reply)
	}
	return nil
}

// validateActivityRule checks that an adapter that lets an Activity hook register
// a session maps some native event to Activity.
func validateActivityRule(d AdapterDescriptor) error {
	if !d.Lifecycle.ActivityMayRegister {
		return nil
	}
	for _, common := range d.Hooks {
		if common == "Activity" {
			return nil
		}
	}
	return fmt.Errorf("%s declares ActivityMayRegister but maps no native event to Activity", d.Kind)
}

// validateHookDeclaration checks what a hook adapter must map.
func validateHookDeclaration(d AdapterDescriptor, hooked bool) error {
	var problems []error
	if hooked && len(d.Hooks) == 0 {
		problems = append(problems, fmt.Errorf("%s implements HookAdapter but maps no native event in Hooks; map at least one, or do not implement HookAdapter", d.Kind))
	}
	if len(d.Hooks) > 0 && !mapsTo(d.Hooks, "SessionStart") {
		problems = append(problems, fmt.Errorf("%s maps no native event to SessionStart; a hook adapter must map one, because core registers a session on it", d.Kind))
	}
	if len(d.Hooks) > 0 && d.HookEventKey == "" {
		problems = append(problems, fmt.Errorf("%s has hooks but declares no HookEventKey; set it in its descriptor to the JSON key its client sends the event name in (for example %q); core has no default", d.Kind, "hook_event_name"))
	}
	return errors.Join(problems...)
}

func mapsTo(hooks map[string]string, event string) bool {
	for _, common := range hooks {
		if common == event {
			return true
		}
	}
	return false
}

// validateHookEventKey checks a declared hook event key. Core reads the event
// name under this key, so it must be a plain name and the adapter must have hooks.
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

// validateNativeDeclaration checks the native hook fields. Core keeps them only
// from the bytes it reads whole, never with the streaming payload selector.
func validateNativeDeclaration(adapter Adapter, d AdapterDescriptor, hooked bool) error {
	if len(d.Native) == 0 {
		return nil
	}
	var problems []error
	if !hooked {
		problems = append(problems, fmt.Errorf("%s declares native hook fields but is not a HookAdapter", d.Kind))
	}
	if payload, ok := adapter.(HookPayloadAdapter); ok && payload.ExtendedHookEvent() != "" {
		problems = append(problems, fmt.Errorf("%s declares native hook fields together with the streaming payload selector (HookPayloadAdapter), which keeps none", d.Kind))
	}
	seen := map[string]bool{}
	for _, field := range d.Native {
		key := strings.ToLower(field.Key)
		if key == "" || seen[key] {
			problems = append(problems, fmt.Errorf("%s: native hook field %q is empty or declared twice (keys match ignoring case)", d.Kind, field.Key))
		}
		seen[key] = true
	}
	return errors.Join(problems...)
}

// validateUncertainWording checks the sentences for an unconfirmed delivery.
// Core uses each one as a format string with the message id: a client declares
// both or neither, and each has exactly one %s and no other verb.
func validateUncertainWording(d AdapterDescriptor) error {
	if d.UncertainReceiver == "" && d.UncertainAcknowledged == "" {
		return nil
	}
	var problems []error
	for _, field := range []struct{ name, sentence string }{{"UncertainReceiver", d.UncertainReceiver}, {"UncertainAcknowledged", d.UncertainAcknowledged}} {
		switch {
		case field.sentence == "":
			problems = append(problems, fmt.Errorf("%s sets only one of UncertainReceiver and UncertainAcknowledged; %s is empty, set both or neither", d.Kind, field.name))
		case strings.Count(field.sentence, "%") != 1 || strings.Count(field.sentence, "%s") != 1:
			problems = append(problems, fmt.Errorf("%s: %s is used as a format string and must have exactly one %%s and no other %% verb, got %q", d.Kind, field.name, field.sentence))
		}
	}
	return errors.Join(problems...)
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

// validateInstallNotice checks the line the install command prints for this adapter.
func validateInstallNotice(d AdapterDescriptor) error {
	if d.InstallNotice != "" && !model.ValidName(d.InstallNotice) {
		return fmt.Errorf("%s declares an InstallNotice that is not one plain line (no control characters, no padding, at most 512 bytes)", d.Kind)
	}
	return nil
}

// commonEvent reports whether name is a common lifecycle event a descriptor may
// map a native name onto. Activity is common but is not in StandardEvents: that
// list is also the set of native names Claude and Codex accept, and "Activity"
// is not a native name for either.
func commonEvent(name string) bool {
	return StandardEvents(true)[name] != "" || name == "Activity"
}
