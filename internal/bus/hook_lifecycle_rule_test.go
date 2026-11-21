package bus

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestLifecycleRulesSetState(t *testing.T) {
	for event, want := range map[string]struct {
		state   model.SessionState
		retired bool
	}{
		"SessionEnd": {model.StateRetired, true}, "PermissionRequest": {model.StateApproval, false},
		"PostToolUse": {model.StateBusy, false}, "UserPromptSubmit": {model.StateBusy, false},
		"Stop": {model.StateReady, false}, "Interrupt": {model.StateReady, false},
	} {
		service := newTestService(t)
		session, _ := register(t, service.Store, "claude")
		if err := service.applyLifecycleState(session.ID, event); err != nil {
			t.Fatal(event, err)
		}
		got, err := service.Store.session(session.ID)
		if err != nil || got.State != want.state || got.Retired != want.retired {
			t.Fatal("lifecycle rule changed", event, got.State, got.Retired, err)
		}
	}
	service := newTestService(t)
	session, _ := register(t, service.Store, "claude")
	if err := service.applyLifecycleState(session.ID, "SessionStart"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatal("an unknown lifecycle event was accepted", err)
	}
}

func TestKeepEndedSessionFollowsTheDescriptor(t *testing.T) {
	keep := agent.Lifecycle{KeepOnSessionEnd: true}
	for _, test := range []struct {
		name    string
		event   string
		life    agent.Lifecycle
		handled bool
	}{
		{"kept end", "SessionEnd", keep, true},
		{"default end retires in the main flow", "SessionEnd", agent.Lifecycle{}, false},
		{"other events are not ends", "Stop", keep, false},
		{"unknown event", "sessionEnd", keep, false},
	} {
		service := newTestService(t)
		session, _ := register(t, service.Store, "codex")
		if err := service.Store.Update(session.ID, model.StateBusy, false); err != nil {
			t.Fatal(err)
		}
		handled, err := service.keepEndedSession(session.ID, test.event, test.life, hookRuntime{store: service.Store, actor: session.ID, kind: "codex", event: test.event})
		got, readErr := service.Store.session(session.ID)
		want := model.StateBusy
		if test.handled {
			want = model.StateReady
		}
		if err != nil || readErr != nil || handled != test.handled || got.State != want || got.Retired {
			t.Fatal(test.name, handled, got.State, got.Retired, err, readErr)
		}
	}
}

// A normalizer cannot allow prompt registration; only the descriptor can.
type forgedPromptAdapter struct {
	stubCursorAdapter
	allow bool
}

func (a forgedPromptAdapter) Descriptor() agent.AdapterDescriptor {
	d := a.stubCursorAdapter.Descriptor()
	d.Lifecycle.PromptMayRegister = a.allow
	return d
}

func (forgedPromptAdapter) NormalizeHook(v agent.HookInput) (agent.HookInput, error) {
	v.AllowPromptRegistration = true
	return v, nil
}

func TestPromptRegistrationComesFromTheDescriptor(t *testing.T) {
	for _, allow := range []bool{false, true} {
		for _, native := range []string{"beforeSubmitPrompt", "sessionStart"} {
			adapter := forgedPromptAdapter{allow: allow}
			service := newTestService(t)
			var trace hookTrace
			input := `{"session_id":"` + model.NewUUID() + `","cwd":"/workspace","hook_event_name":"` + native + `"}`
			event, _, err := service.decodeHook(context.Background(), "cursor", strings.NewReader(input), adapter, adapter, &trace)
			if err != nil {
				t.Fatal(err)
			}
			if want := allow && native == "beforeSubmitPrompt"; event.AllowPromptRegistration != want {
				t.Fatal("prompt registration was not decided by the descriptor", allow, native, event.AllowPromptRegistration)
			}
		}
	}
}

// countingHooks records whether core ran the adapter's own handler.
type countingHooks struct {
	agent.StandardHooks
	calls int
}

func (h *countingHooks) HandleHook(agent.HookHost, agent.HookInput, string, io.Writer) (bool, error) {
	h.calls++
	return false, nil
}

// A failed write for a kept session end must reach the caller, and the
// adapter's handler must not run after it.
func TestKeptEndWriteErrorStopsTheHook(t *testing.T) {
	service := newTestService(t)
	session, _ := register(t, service.Store, "codex")
	stale := hookRuntime{store: service.Store, actor: session.ID, kind: "codex", event: "SessionEnd", generation: session.Generation + "-old", generationBound: true}
	hooks := &countingHooks{}
	handled, err := service.runHookHandler(hooks, stale, agent.HookInput{Event: "SessionEnd"}, session.ID, agent.Lifecycle{KeepOnSessionEnd: true}, io.Discard)
	if !errors.Is(err, errStaleHook) || handled || hooks.calls != 0 {
		t.Fatal("a failed kept-end write was dropped or the handler still ran", handled, hooks.calls, err)
	}
	fresh := hookRuntime{store: service.Store, actor: session.ID, kind: "codex", event: "SessionEnd", generation: session.Generation, generationBound: true}
	handled, err = service.runHookHandler(hooks, fresh, agent.HookInput{Event: "SessionEnd"}, session.ID, agent.Lifecycle{KeepOnSessionEnd: true}, io.Discard)
	if err != nil || !handled || hooks.calls != 1 {
		t.Fatal("a kept end must be handled and still run the adapter's handler", handled, hooks.calls, err)
	}
}
