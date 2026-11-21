// Package agenttest checks that an adapter follows the contracts of package
// agent. It is test support and is never imported by product code.
package agenttest

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// events are the common lifecycle events a descriptor may map native names onto.
var events = map[string]bool{
	"SessionStart": true, "SessionEnd": true, "PermissionRequest": true, "PostToolUse": true,
	"UserPromptSubmit": true, "Stop": true, "Interrupt": true,
}

type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// Run checks an adapter's descriptor, hooks, state codec and capabilities.
func Run(t *testing.T, adapter agent.Adapter) {
	t.Helper()
	t.Run("descriptor", func(t *testing.T) { checkDescriptor(t, adapter) })
	t.Run("hooks", func(t *testing.T) { checkHooks(t, adapter) })
	t.Run("state", func(t *testing.T) { checkState(t, adapter) })
	t.Run("capabilities", func(t *testing.T) { checkCapabilities(t, adapter) })
}

func checkDescriptor(t reporter, adapter agent.Adapter) {
	t.Helper()
	d := adapter.Descriptor()
	if d.Kind == "" || d.Label == "" {
		t.Errorf("descriptor needs a kind and a label")
	}
	_, hooked := adapter.(agent.HookAdapter)
	if hooked && len(d.Hooks) == 0 {
		t.Errorf("a hook adapter must map at least one native event")
	}
	if !hooked && len(d.Hooks) > 0 {
		t.Errorf("descriptor maps hooks but the adapter does not implement HookAdapter")
	}
	for native, common := range d.Hooks {
		if native == "" || !events[common] {
			t.Errorf("hook %q maps to the unknown event %q", native, common)
		}
	}
	if hooked && !mapsTo(d.Hooks, "SessionStart") {
		t.Errorf("a hook adapter must map a native event to SessionStart")
	}
}

func mapsTo(hooks map[string]string, event string) bool {
	for _, common := range hooks {
		if common == event {
			return true
		}
	}
	return false
}

func checkHooks(t reporter, adapter agent.Adapter) {
	t.Helper()
	hooks, ok := adapter.(agent.HookAdapter)
	if !ok {
		return
	}
	for event := range events {
		var out agent.HookInput
		var err error
		if message := panicked(func() {
			out, err = hooks.NormalizeHook(agent.HookInput{Event: event, SessionID: "fixture", CWD: "/fixture"})
		}); message != nil {
			t.Errorf("%s: NormalizeHook panicked: %v", event, message)
			continue
		}
		if err != nil {
			continue
		}
		if out.AllowPromptRegistration {
			t.Errorf("%s: NormalizeHook must not allow prompt registration; the descriptor does", event)
		}
		if !events[out.Event] {
			t.Errorf("%s: NormalizeHook returned the unknown event %q", event, out.Event)
		}
		if message := panicked(func() { _, _ = hooks.HandleHook(host{}, out, "fixture", io.Discard) }); message != nil {
			t.Errorf("%s: HandleHook panicked: %v", event, message)
		}
	}
}

func checkState(t reporter, adapter agent.Adapter) {
	t.Helper()
	codec, ok := adapter.(agent.SessionStateAdapter)
	if !ok {
		return
	}
	first, err := codec.DecodeState(agent.StateInput{})
	if err != nil {
		return
	}
	second, err := codec.DecodeState(agent.StateInput{State: first})
	if err != nil || !sameJSON(first, second) {
		t.Errorf("DecodeState must accept its own output unchanged: %v", err)
	}
}

func sameJSON(a, b json.RawMessage) bool {
	var left, right bytes.Buffer
	return json.Compact(&left, a) == nil && json.Compact(&right, b) == nil && left.String() == right.String()
}

func checkCapabilities(t reporter, adapter agent.Adapter) {
	t.Helper()
	caps := adapter.ReceiveCapabilities(model.Session{})
	if _, ok := adapter.(agent.HookAdapter); caps.LifecycleHooks && !ok {
		t.Errorf("capabilities advertise lifecycle hooks but the adapter has no HookAdapter")
	}
}

// panicked returns the value a call panicked with, or nil. A bad adapter then
// fails its own check instead of stopping the whole test run.
func panicked(call func()) (message any) {
	defer func() { message = recover() }()
	call()
	return nil
}

// host is a lifecycle host that changes nothing and finds no session.
type host struct{}

func (host) UpdateSession(string, model.SessionState, bool) error { return nil }
func (host) EmitPending(string) (string, error)                   { return "", os.ErrNotExist }
func (host) CompletedNativePrompt(string, string) (string, error) { return "", nil }
func (host) ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error)) error {
	return nil
}
func (host) ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error)) (agent.RecoveryGrant, error) {
	return agent.RecoveryGrant{}, nil
}
