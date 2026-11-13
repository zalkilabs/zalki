package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func cursorHost(store Store, actor, event string) hookRuntime {
	return hookRuntime{store: store, actor: actor, kind: "cursor", event: event}
}

func sessionBytes(t *testing.T, store Store, id string) []byte {
	t.Helper()
	path, err := store.sessionPath(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func encodedCursorState(state CursorListenerState) json.RawMessage {
	v := model.Session{}
	setCursorState(&v, state)
	return v.AdapterState
}

func TestLifecycleTransitionsAreScopedToTheHookEvent(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	for _, test := range []struct {
		event      string
		transition agent.LifecycleTransition
	}{
		{"PostToolUse", agent.TransitionPromptReactivate}, {"Stop", agent.TransitionPromptReactivate},
		{"UserPromptSubmit", agent.TransitionReady}, {"PostToolUse", agent.TransitionReady},
		{"SessionStart", agent.TransitionNone}, {"SessionEnd", agent.TransitionNone}, {"PermissionRequest", agent.TransitionNone},
	} {
		before := sessionBytes(t, store, to.ID)
		err := cursorHost(store, to.ID, test.event).ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
			return agent.LifecycleChange{Transition: test.transition, AdapterState: encodedCursorState(CursorListenerState{RecoveryAttempted: true})}, nil
		})
		if model.ErrorCodeOf(err) != model.CodeInvalidTransition || !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
			t.Fatalf("%s accepted transition %d: %v", test.event, test.transition, err)
		}
	}
	for _, event := range []string{"UserPromptSubmit", "SessionStart", "SessionEnd", "PermissionRequest"} {
		called := false
		grant, err := cursorHost(store, to.ID, event).ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
			called = true
			return agent.RecoveryDecision{Recover: true}, nil
		})
		if model.ErrorCodeOf(err) != model.CodeInvalidTransition || called || grant.Capability != "" {
			t.Fatalf("%s claimed recovery: %v", event, err)
		}
	}
	if _, err := store.authenticate(to.ID, key); err != nil {
		t.Fatal("a rejected recovery changed the credential", err)
	}
}

func TestLifecyclePolicyFailuresWriteNothing(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	policyErr := errors.New("policy failed")
	invalid := map[string]json.RawMessage{
		"policy error":       nil,
		"oversized state":    json.RawMessage(`{"schema":1,"pad":"` + strings.Repeat("x", maxAdapterState) + `"}`),
		"malformed JSON":     json.RawMessage(`{"schema":1`),
		"unsupported schema": json.RawMessage(`{"schema":2}`),
		"null state":         json.RawMessage(`null`),
	}
	for name, state := range invalid {
		before := sessionBytes(t, store, to.ID)
		err := cursorHost(store, to.ID, "UserPromptSubmit").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
			if state == nil {
				return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate}, policyErr
			}
			return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate, AdapterState: state}, nil
		})
		if err == nil || !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
			t.Fatalf("lifecycle %s was written: %v", name, err)
		}
		grant, err := cursorHost(store, to.ID, "PostToolUse").ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
			if state == nil {
				return agent.RecoveryDecision{Recover: true}, policyErr
			}
			return agent.RecoveryDecision{Recover: true, AdapterState: state}, nil
		})
		if err == nil || grant.Capability != "" || !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
			t.Fatalf("recovery %s rotated or wrote state: %v", name, err)
		}
	}
	before := sessionBytes(t, store, to.ID)
	grant, err := cursorHost(store, to.ID, "PostToolUse").ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
		return agent.RecoveryDecision{AdapterState: encodedCursorState(CursorListenerState{RecoveryAttempted: true})}, nil
	})
	if err != nil || grant.Capability != "" || !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
		t.Fatal("a declined recovery persisted its state", err)
	}
	if _, err := store.authenticate(to.ID, key); err != nil {
		t.Fatal("a failed recovery changed the credential", err)
	}
}

func TestLifecyclePolicyCannotReachTheJournalThroughAliases(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	if err := cursorHost(store, to.ID, "Stop").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{AdapterState: encodedCursorState(CursorListenerState{RecoveryAttempted: true})}, nil
	}); err != nil {
		t.Fatal(err)
	}
	before := sessionBytes(t, store, to.ID)
	var retained model.SessionView
	if err := cursorHost(store, to.ID, "Stop").ApplyLifecycle(func(v model.SessionView) (agent.LifecycleChange, error) {
		if len(v.AdapterState) == 0 {
			t.Fatal("view omitted the adapter state")
		}
		for i := range v.AdapterState {
			v.AdapterState[i] = ' '
		}
		retained = v
		return agent.LifecycleChange{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
		t.Fatal("mutating the view changed the journal")
	}
	retained.AdapterState = append(retained.AdapterState[:0], `{"schema":1,"recovery_suppressed":true}`...)
	if current, err := store.session(to.ID); err != nil || cursorState(current).RecoverySuppressed || !cursorState(current).RecoveryAttempted {
		t.Fatal("a retained view changed the stored session", err)
	}

	next := encodedCursorState(CursorListenerState{RecoverySuppressed: true})
	if err := cursorHost(store, to.ID, "Stop").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{AdapterState: next}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := range next {
		next[i] = ' '
	}
	if current, err := store.session(to.ID); err != nil || !cursorState(current).RecoverySuppressed {
		t.Fatal("mutating a returned state after the transaction changed the session", err)
	}
}

func TestLifecycleHostIsBoundToItsSessionKind(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	before := sessionBytes(t, store, to.ID)
	wrong := hookRuntime{store: store, actor: to.ID, kind: "codex", event: "UserPromptSubmit"}
	if err := wrong.ApplyLifecycle(stubResumeOnPrompt); model.ErrorCodeOf(err) != model.CodeInvalidIdentity {
		t.Fatal("another agent's hook changed a Cursor session", err)
	}
	wrong.event = "PostToolUse"
	if grant, err := wrong.ClaimRecovery(stubClaimRecovery); model.ErrorCodeOf(err) != model.CodeInvalidIdentity || grant.Capability != "" {
		t.Fatal("another agent's hook reissued a Cursor credential", err)
	}
	if !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
		t.Fatal("a mismatched hook wrote the session")
	}
	if _, err := store.authenticate(to.ID, key); err != nil {
		t.Fatal(err)
	}

	unknown := "cursor:" + newID()
	called := false
	if err := cursorHost(store, unknown, "UserPromptSubmit").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		called = true
		return agent.LifecycleChange{}, nil
	}); err != nil || called {
		t.Fatal("an unknown session ran a lifecycle policy", err)
	}
	if path, _ := store.sessionPath(unknown); fileExists(path) {
		t.Fatal("an unknown session was created by a lifecycle transaction")
	}
}

func TestStopTransitionKeepsARetiredSessionRetired(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	if err := store.Update(to.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if err := cursorHost(store, to.ID, "Stop").ApplyLifecycle(stubSuppressRecovery); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(to.ID)
	if err != nil || !current.Retired || current.State != model.StateRetired || !cursorState(current).RecoverySuppressed {
		t.Fatalf("stop revived a retired session or dropped its adapter state: %+v %v", current, err)
	}
}

func TestRecoveryGuardsRunBeforeThePolicy(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	for _, state := range []model.SessionState{model.StateApproval, model.StateRetired} {
		if err := store.Update(to.ID, state, state == model.StateRetired); err != nil {
			t.Fatal(err)
		}
		called := false
		grant, err := cursorHost(store, to.ID, "PostToolUse").ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
			called = true
			return agent.RecoveryDecision{Recover: true}, nil
		})
		if err != nil || called || grant.Capability != "" {
			t.Fatalf("a %s session reached the recovery policy: %v", state, err)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// rewritingCursorAdapter's codec replaces whatever state it is asked to accept.
type rewritingCursorAdapter struct {
	stubCursorAdapter
	rewrite json.RawMessage
}

func (a rewritingCursorAdapter) DecodeState(in agent.StateInput) (json.RawMessage, error) {
	if len(in.State) == 0 {
		return a.stubCursorAdapter.DecodeState(in)
	}
	return a.rewrite, nil
}

func TestCodecResultIsBoundedBeforePersisting(t *testing.T) {
	for name, rewrite := range map[string]json.RawMessage{
		"oversized": json.RawMessage(`{"schema":1,"pad":"` + strings.Repeat("x", maxAdapterState) + `"}`),
		"malformed": json.RawMessage(`{"schema":1`),
		"empty":     {},
	} {
		t.Run(name, func(t *testing.T) {
			store := fixtureStore(t)
			to, key := register(t, store, "cursor")
			// A supported legacy record has no persisted adapter state. Reads
			// initialize it normally; only returned policy state is rewritten.
			to.AdapterState = nil
			if err := store.locked(func() error { return store.saveSession(to) }); err != nil {
				t.Fatal(err)
			}
			registry, err := agent.NewRegistry(stubCodexAdapter{}, rewritingCursorAdapter{rewrite: rewrite})
			if err != nil {
				t.Fatal(err)
			}
			store.Registry = registry
			before := sessionBytes(t, store, to.ID)
			if err := cursorHost(store, to.ID, "UserPromptSubmit").ApplyLifecycle(stubResumeOnPrompt); model.ErrorCodeOf(err) != model.CodeInvalidInput {
				t.Fatal("lifecycle persisted an unbounded codec result", err)
			}
			if grant, err := cursorHost(store, to.ID, "PostToolUse").ClaimRecovery(stubClaimRecovery); model.ErrorCodeOf(err) != model.CodeInvalidInput || grant.Capability != "" {
				t.Fatal("recovery persisted an unbounded codec result", err)
			}
			if !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
				t.Fatal("a rejected codec result changed the record")
			}
			if _, err := store.authenticate(to.ID, key); err != nil {
				t.Fatal("a rejected codec result changed the credential", err)
			}
		})
	}
}
