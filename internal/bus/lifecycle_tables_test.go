package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// These tests pin the tables that decide what each common event does: the state
// rule, the event cause, the transitions a policy may request and the events that
// may claim a recovery.

func TestLifecycleRulesAreExactlyTheCurrentRows(t *testing.T) {
	want := map[string]lifecycleRule{
		"SessionEnd":        {state: model.StateRetired, retire: true, endsSession: true},
		"PermissionRequest": {state: model.StateApproval},
		"PostToolUse":       {state: model.StateBusy},
		"Activity":          {state: model.StateBusy},
		"UserPromptSubmit":  {state: model.StateBusy},
		"Stop":              {state: model.StateReady},
		"Interrupt":         {state: model.StateReady},
	}
	if !reflect.DeepEqual(lifecycleRules, want) {
		t.Fatalf("lifecycleRules = %+v, want %+v", lifecycleRules, want)
	}
}

func TestLifecycleCausesAreExactlyTheCurrentRows(t *testing.T) {
	want := map[string]model.EventCause{
		"Stop": model.CauseStop, "Interrupt": model.CauseInterrupt, "UserPromptSubmit": model.CausePrompt,
		"PostToolUse": model.CauseTool, "PermissionRequest": model.CausePermission, "SessionEnd": model.CauseSessionEnd,
		"Activity": model.CauseActivity,
	}
	if !reflect.DeepEqual(lifecycleCauses, want) {
		t.Fatalf("lifecycleCauses = %v, want %v", lifecycleCauses, want)
	}
	if lifecycleCause("SessionStart") != "" || lifecycleCause("UnknownEvent") != "" {
		t.Fatal("an event outside the table has a cause")
	}
}

func TestAllowedTransitionsAreExactlyTheCurrentRows(t *testing.T) {
	want := map[string][]agent.LifecycleTransition{
		"UserPromptSubmit": {agent.TransitionNone, agent.TransitionPromptReactivate},
		"Stop":             {agent.TransitionNone, agent.TransitionReady},
		"Interrupt":        {agent.TransitionNone, agent.TransitionReady},
		"PostToolUse":      {agent.TransitionNone},
		"Activity":         {agent.TransitionNone, agent.TransitionPromptReactivate},
	}
	if !reflect.DeepEqual(allowedTransitions, want) {
		t.Fatalf("allowedTransitions = %v, want %v", allowedTransitions, want)
	}
}

func TestRecoveryEventsAreExactlyToolActivityAndStop(t *testing.T) {
	if want := map[string]bool{"PostToolUse": true, "Activity": true, "Stop": true}; !reflect.DeepEqual(recoveryEvents, want) {
		t.Fatalf("recoveryEvents = %v, want %v", recoveryEvents, want)
	}
}

// ClaimRecovery refuses every other event before it looks at the session.
func TestClaimRecoveryIsRefusedOutsideItsEvents(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "cursor")
	for _, event := range []string{"SessionStart", "SessionEnd", "PermissionRequest", "UserPromptSubmit", "Interrupt", "UnknownEvent"} {
		called := false
		_, err := cursorHost(store, session.ID, event).ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
			called = true
			return agent.RecoveryDecision{}, nil
		})
		if model.ErrorCodeOf(err) != model.CodeInvalidTransition || called {
			t.Errorf("%s: error %v, policy called %v; want a refused transition", event, err, called)
		}
	}
	for _, event := range []string{"PostToolUse", "Activity", "Stop"} {
		if _, err := cursorHost(store, session.ID, event).ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
			return agent.RecoveryDecision{}, nil
		}); err != nil {
			t.Errorf("%s: %v", event, err)
		}
	}
}

// Transitions are scoped to the hook event: an event outside the table allows none.
func TestNoTransitionIsAllowedForAnEventOutsideTheTable(t *testing.T) {
	for _, event := range []string{"SessionStart", "SessionEnd", "PermissionRequest", "UnknownEvent"} {
		for _, transition := range []agent.LifecycleTransition{agent.TransitionNone, agent.TransitionReady, agent.TransitionPromptReactivate} {
			if transitionAllowed(event, transition) {
				t.Errorf("%s allows transition %v", event, transition)
			}
		}
	}
}

// An Activity hook may reactivate a retired session like a prompt, and may not
// mark a turn ready (that is what a stop does).
func TestActivityAllowsNoneAndReactivateButNotReady(t *testing.T) {
	if !transitionAllowed("Activity", agent.TransitionNone) || !transitionAllowed("Activity", agent.TransitionPromptReactivate) || transitionAllowed("Activity", agent.TransitionReady) {
		t.Fatal("the transitions of an Activity hook changed")
	}
}

// hookEvent runs one hook of the claude stub adapter for a session.
func hookEvent(t *testing.T, service Service, native, event string) error {
	t.Helper()
	input, err := json.Marshal(map[string]any{"hook_event_name": event, "session_id": native, "cwd": "/workspace", "prompt": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := service.Hook(context.Background(), "claude", bytes.NewReader(input), &out); err != nil {
		return err
	}
	if out.Len() != 0 {
		t.Fatalf("%s wrote %q", event, out.String())
	}
	return nil
}

// Core never revives a retired session on its own: only an adapter policy can,
// and only on a prompt. A hook of any other event, and a prompt of an adapter
// without a policy, leave it retired.
func TestARetiredSessionStaysRetiredForEveryEventWithoutAPolicy(t *testing.T) {
	for _, event := range []string{"PostToolUse", "Stop", "PermissionRequest", "UserPromptSubmit"} {
		service := newTestService(t)
		session, _ := register(t, service.Store, "claude")
		if err := service.Store.Update(session.ID, model.StateRetired, true); err != nil {
			t.Fatal(err)
		}
		if err := hookEvent(t, service, session.NativeID, event); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		got, err := service.Store.session(session.ID)
		if err != nil || !got.Retired || got.State != model.StateRetired {
			t.Errorf("%s revived a retired session: retired %v, state %q, error %v", event, got.Retired, got.State, err)
		}
	}
}

// Which events refresh the owner process of a session: only a prompt does. A
// recorded owner that has exited is dropped by a prompt and kept by every other event.
func TestOnlyAPromptRefreshesTheOwner(t *testing.T) {
	for _, test := range []struct {
		event      string
		wantKept   bool
		retiredToo bool
	}{
		{"UserPromptSubmit", false, false}, {"UserPromptSubmit", false, true},
		{"PostToolUse", true, false}, {"Stop", true, false}, {"PermissionRequest", true, false},
	} {
		service := newTestService(t)
		session, _ := register(t, service.Store, "claude")
		exitedOwner(t, service.Store, session)
		if test.retiredToo {
			if err := service.Store.Update(session.ID, model.StateRetired, true); err != nil {
				t.Fatal(err)
			}
		}
		if err := hookEvent(t, service, session.NativeID, test.event); err != nil {
			t.Fatalf("%s: %v", test.event, err)
		}
		got, err := service.Store.session(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if kept := got.Owner != nil; kept != test.wantKept {
			t.Errorf("%s (retired %v): the exited owner was kept = %v, want %v", test.event, test.retiredToo, kept, test.wantKept)
		}
	}
}
