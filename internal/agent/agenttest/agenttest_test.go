package agenttest

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type recorder struct{ failures []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// brokenAdapter breaks one rule at a time so each check proves it can fail.
type brokenAdapter struct {
	agent.StandardHooks
	descriptor agent.AdapterDescriptor
	normalize  func(agent.HookInput) (agent.HookInput, error)
	handle     func() (bool, error)
	caps       model.ReceiveCapabilities
}

func (a brokenAdapter) Descriptor() agent.AdapterDescriptor { return a.descriptor }
func (a brokenAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return a.caps
}
func (brokenAdapter) Deliver(_ context.Context, _ model.Message, _ model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}
func (a brokenAdapter) NormalizeHook(v agent.HookInput) (agent.HookInput, error) {
	if a.normalize != nil {
		return a.normalize(v)
	}
	return v, nil
}
func (a brokenAdapter) HandleHook(agent.HookHost, agent.HookInput, string, io.Writer) (bool, error) {
	if a.handle != nil {
		return a.handle()
	}
	return false, nil
}

func good() brokenAdapter {
	return brokenAdapter{descriptor: agent.AdapterDescriptor{Kind: "fixture", Label: "Fixture", Hooks: agent.StandardEvents(true)}, caps: model.ReceiveCapabilities{LifecycleHooks: true}}
}

func TestChecksCatchBrokenAdapters(t *testing.T) {
	for _, test := range []struct {
		name   string
		break_ func(*brokenAdapter)
		want   string
	}{
		{"missing label", func(a *brokenAdapter) { a.descriptor.Label = "" }, "kind and a label"},
		{"unknown mapped event", func(a *brokenAdapter) { a.descriptor.Hooks = map[string]string{"start": "SessionStart", "x": "Nope"} }, "unknown event"},
		{"no session start", func(a *brokenAdapter) { a.descriptor.Hooks = map[string]string{"stop": "Stop"} }, "SessionStart"},
		{"no hooks mapped", func(a *brokenAdapter) { a.descriptor.Hooks = nil }, "at least one"},
		{"normalizer allows registration", func(a *brokenAdapter) {
			a.normalize = func(v agent.HookInput) (agent.HookInput, error) { v.AllowPromptRegistration = true; return v, nil }
		}, "descriptor does"},
		{"normalizer invents an event", func(a *brokenAdapter) {
			a.normalize = func(v agent.HookInput) (agent.HookInput, error) { v.Event = "Other"; return v, nil }
		}, "unknown event"},
		{"normalizer panics", func(a *brokenAdapter) {
			a.normalize = func(agent.HookInput) (agent.HookInput, error) { panic("boom") }
		}, "NormalizeHook panicked"},
		{"handler panics", func(a *brokenAdapter) { a.handle = func() (bool, error) { panic("boom") } }, "HandleHook panicked"},
	} {
		adapter := good()
		test.break_(&adapter)
		r := &recorder{}
		checkDescriptor(r, adapter)
		checkHooks(r, adapter)
		if !strings.Contains(strings.Join(r.failures, "\n"), test.want) {
			t.Fatal(test.name, "was not caught:", r.failures)
		}
	}
	r := &recorder{}
	checkDescriptor(r, good())
	checkHooks(r, good())
	checkCapabilities(r, good())
	if len(r.failures) != 0 {
		t.Fatal("a conforming adapter failed:", r.failures)
	}
}

func TestCapabilitiesNeedHooks(t *testing.T) {
	r := &recorder{}
	checkCapabilities(r, bareAdapter{})
	if len(r.failures) != 1 {
		t.Fatal("hook capabilities without HookAdapter were not caught", r.failures)
	}
}

type bareAdapter struct{}

func (bareAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "bare", Label: "Bare"}
}
func (bareAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{LifecycleHooks: true}
}
func (bareAdapter) Deliver(_ context.Context, _ model.Message, _ model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}
