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
	return brokenAdapter{
		descriptor: agent.AdapterDescriptor{Kind: "fixture", Label: "Fixture", HookEventKey: "hook_event_name", Hooks: agent.StandardEvents(true)},
		caps:       model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, LifecycleHooks: true},
	}
}

func TestChecksCatchBrokenAdapters(t *testing.T) {
	for _, test := range []struct {
		name   string
		break_ func(*brokenAdapter)
		want   string
	}{
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
		checkHooks(r, adapter)
		if !strings.Contains(strings.Join(r.failures, "\n"), test.want) {
			t.Fatal(test.name, "was not caught:", r.failures)
		}
	}
	r := &recorder{}
	checkDeclaration(r, good())
	checkHooks(r, good())
	checkState(r, good())
	if len(r.failures) != 0 {
		t.Fatal("a conforming adapter failed:", r.failures)
	}
}

// The declaration rules are not here: the registry enforces them (see the
// registry tests in package agent) and Run reports its refusal, every problem
// of the adapter at once.
func TestRunReportsTheRegistryRefusal(t *testing.T) {
	adapter := good()
	adapter.descriptor.HookEventKey = ""
	adapter.descriptor.Hooks = map[string]string{"stop": "Stop"}
	r := &recorder{}
	checkDeclaration(r, adapter)
	if len(r.failures) != 1 {
		t.Fatalf("want one failure that lists the problems, got %q", r.failures)
	}
	for _, want := range []string{"the registry refuses this adapter's declaration", "fixture maps no native event to SessionStart", "fixture has hooks but declares no HookEventKey"} {
		if !strings.Contains(r.failures[0], want) {
			t.Fatalf("failure lacks %q:\n%s", want, r.failures[0])
		}
	}
}
