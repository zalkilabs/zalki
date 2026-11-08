// These boundary cases pin snapshot detachment and existing guard precedence.
package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionViewBuilderDetachesAdapterState(t *testing.T) {
	source := model.Session{ID: "cursor:id", Kind: "cursor", Generation: "gen", State: model.StateBusy, Retired: true, AdapterState: json.RawMessage(`{"v":1}`), KeyHash: "private hash"}
	view := viewOf(source)
	if view.Actor != "cursor:id" || view.Kind != "cursor" || view.Generation != "gen" || view.State != model.StateBusy || !view.Retired || string(view.AdapterState) != `{"v":1}` {
		t.Fatalf("view fields changed: %+v", view)
	}
	view.AdapterState[5] = '2'
	if string(source.AdapterState) != `{"v":1}` {
		t.Fatal("view mutation reached the source session")
	}
	source.AdapterState[5] = '3'
	if string(view.AdapterState) != `{"v":2}` {
		t.Fatal("source mutation reached the detached view")
	}
}

func TestLifecyclePolicyErrorPrecedesTransitionValidation(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "cursor")
	before := sessionBytes(t, store, target.ID)
	policyErr := errors.New("policy failed before transition validation")
	err := cursorHost(store, target.ID, "PostToolUse").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate}, policyErr
	})
	if !errors.Is(err, policyErr) {
		t.Fatalf("first error=%v, want policy error", err)
	}
	if !bytes.Equal(before, sessionBytes(t, store, target.ID)) {
		t.Fatal("policy error wrote the session")
	}
}

func TestLifecycleTransitionErrorPrecedesStateValidation(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "cursor")
	before := sessionBytes(t, store, target.ID)
	err := cursorHost(store, target.ID, "PostToolUse").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate, AdapterState: json.RawMessage(`{`)}, nil
	})
	if err == nil || err.Error() != "invalid_transition: lifecycle transition is not allowed for this hook event" {
		t.Fatalf("first error=%v, want event transition error", err)
	}
	if !bytes.Equal(before, sessionBytes(t, store, target.ID)) {
		t.Fatal("transition error wrote malformed state")
	}
}

func TestRecoveryEventErrorPrecedesActorLookup(t *testing.T) {
	store := fixtureStore(t)
	host := hookRuntime{store: store, actor: "invalid actor", kind: "cursor", event: "UserPromptSubmit"}
	called := false
	grant, err := host.ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
		called = true
		return agent.RecoveryDecision{Recover: true}, nil
	})
	if err == nil || err.Error() != "invalid_transition: recovery is not allowed for this hook event" {
		t.Fatalf("first error=%v, want recovery event error", err)
	}
	if called || grant.Capability != "" {
		t.Fatal("forbidden event reached policy or granted a capability")
	}
}
