package antigravity

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestDescriptorDeclaresWhatAgyProved(t *testing.T) {
	d := New().Descriptor()
	if d.Kind != "antigravity" || d.Label != "Antigravity" || d.HookEventKey != "hookEventName" || d.Executable != "agy" || d.OwnerRole != "client" {
		t.Fatalf("descriptor identity = %+v", d)
	}
	if want := []string{"agy"}; !reflect.DeepEqual(d.ProcessNames, want) {
		t.Fatalf("process names = %v, want %v: a shared app process must not own a chat", d.ProcessNames, want)
	}
	// Three hooks only: a tool hook that fails would block the user's tool.
	want := map[string]string{"SessionStart": "SessionStart", "PreInvocation": "Activity", "Stop": "Stop"}
	if !reflect.DeepEqual(d.Hooks, want) {
		t.Fatalf("hooks = %v, want %v", d.Hooks, want)
	}
	if want := []agent.NativeField{{Key: "conversationId", Kind: agent.NativeString, Capture: agent.CaptureID}, {Key: "terminationReason", Kind: agent.NativeString}}; !reflect.DeepEqual(d.Native, want) {
		t.Fatalf("native fields = %+v", d.Native)
	}
	if d.Lifecycle != (agent.Lifecycle{ActivityMayRegister: true}) || d.UncertainReceiver != "" || d.UncertainAcknowledged != "" {
		t.Fatalf("lifecycle %+v and wording are not the defaults", d)
	}
}

func TestCapabilitiesAreHookDeliveryWithoutIdleWake(t *testing.T) {
	want := model.ReceiveCapabilities{Delivery: "next_invocation_or_stop_hook", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, LifecycleHooks: true, HookDelivery: true}
	if got := New().ReceiveCapabilities(model.Session{}); got != want {
		t.Fatalf("capabilities = %+v, want %+v", got, want)
	}
	if receipt := New().Deliver(t.Context(), model.Message{}, model.Session{}); receipt.Status != model.StatusPendingActivity {
		t.Fatalf("receipt status = %q, want %q: there is no live path to a chat", receipt.Status, model.StatusPendingActivity)
	}
	if transport, listener := New().Health(nil, model.Session{}); transport != "deferred" || listener != "" {
		t.Fatalf("health = %q, %q", transport, listener)
	}
}

func hookInput(event, native string) agent.HookInput {
	return agent.HookInput{Event: event, Native: json.RawMessage(native)}
}

func TestNormalizeHookTakesTheSessionIDFromTheConversation(t *testing.T) {
	got, err := New().NormalizeHook(hookInput("PostToolUse", `{"conversationId":"c-1"}`))
	if err != nil || got.SessionID != "c-1" || got.Event != "PostToolUse" {
		t.Fatalf("normalized = %+v, %v", got, err)
	}
	if got, _ := New().NormalizeHook(hookInput("SessionStart", "")); got.SessionID != "" {
		t.Fatalf("a payload without a conversation id got the session id %q", got.SessionID)
	}
}

func TestNormalizeHookTurnsOnlyAnAbnormalStopIntoAnInterrupt(t *testing.T) {
	for reason, want := range map[string]string{
		"NO_TOOL_CALL": "Stop", "": "Stop", "USER_CANCELED": "Interrupt", "ERROR": "Interrupt", "MAX_INVOCATIONS": "Interrupt",
		"MAX_FORCED_INVOCATIONS": "Interrupt", "MAX_TOKEN_BUDGET_EXCEEDED": "Interrupt", "TERMINAL_STEP_TYPE": "Interrupt",
		"TERMINAL_CUSTOM_HOOK": "Interrupt", "INJECTED_RESPONSE": "Interrupt",
	} {
		native := `{"conversationId":"c-1"}`
		if reason != "" {
			native = `{"conversationId":"c-1","terminationReason":"` + reason + `"}`
		}
		got, err := New().NormalizeHook(hookInput("Stop", native))
		if err != nil || got.Event != want {
			t.Errorf("Stop with reason %q became %q, want %q (%v)", reason, got.Event, want, err)
		}
	}
	// Only a Stop is looked at.
	if got, _ := New().NormalizeHook(hookInput("PostToolUse", `{"terminationReason":"ERROR"}`)); got.Event != "PostToolUse" {
		t.Fatalf("a model call with a reason became %q", got.Event)
	}
}

func TestStateCountsContinuesAndRefusesNonsense(t *testing.T) {
	adapter := New()
	empty, err := adapter.DecodeState(agent.StateInput{})
	if err != nil || string(empty) != `{}` {
		t.Fatalf("empty state = %s, %v", empty, err)
	}
	kept, err := adapter.DecodeState(agent.StateInput{State: json.RawMessage(`{"continues":3}`)})
	if err != nil || string(kept) != `{"continues":3}` {
		t.Fatalf("state = %s, %v", kept, err)
	}
	for _, raw := range []string{`{"continues":-1}`, `{"continues":6}`, `{"continues":99}`} {
		if got, err := adapter.DecodeState(agent.StateInput{State: json.RawMessage(raw)}); err != nil || string(got) != `{}` {
			t.Errorf("state %s became %s (%v), want the count reset", raw, got, err)
		}
	}
	if _, err := adapter.DecodeState(agent.StateInput{State: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("a state that is not an object was accepted")
	}
}
