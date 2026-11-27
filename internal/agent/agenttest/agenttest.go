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
var events = agent.StandardEvents(true)

type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// Run checks an adapter's declaration, hooks and state codec. The declaration
// is checked by the registry itself (agent.NewRegistry), the same code core runs
// at start-up, so no declaration rule exists only here. The hooks and state
// checks are behavior checks: they call the adapter's own code with fixtures,
// which production construction must not do.
func Run(t *testing.T, adapter agent.Adapter) {
	t.Helper()
	t.Run("declaration", func(t *testing.T) { checkDeclaration(t, adapter) })
	t.Run("hooks", func(t *testing.T) { checkHooks(t, adapter) })
	t.Run("state", func(t *testing.T) { checkState(t, adapter) })
}

// checkDeclaration fails with every problem the registry finds, one per line.
func checkDeclaration(t reporter, adapter agent.Adapter) {
	t.Helper()
	if _, err := agent.NewRegistry(adapter); err != nil {
		t.Errorf("the registry refuses this adapter's declaration:\n%v", err)
	}
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
		if events[out.Event] == "" {
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
