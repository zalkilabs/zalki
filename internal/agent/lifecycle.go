package agent

import (
	"encoding/json"

	"github.com/zalkilabs/zalki/internal/model"
)

// LifecycleTransition names a shared-state change core performs on behalf of
// an adapter. Which transitions a hook may request depends on its event.
type LifecycleTransition int

const (
	TransitionNone LifecycleTransition = iota
	// TransitionPromptReactivate resumes a conversation on an actual prompt,
	// including one retired by a previous session end.
	TransitionPromptReactivate
	// TransitionReady marks a stopped turn ready without reactivating a retired one.
	TransitionReady
)

// LifecycleChange is an adapter policy result. A nil AdapterState keeps the
// current state.
type LifecycleChange struct {
	Transition   LifecycleTransition
	AdapterState json.RawMessage
}

// RecoveryDecision is an adapter's recovery policy result. AdapterState is
// persisted only together with the reissued credential when Recover is set.
type RecoveryDecision struct {
	Recover      bool
	AdapterState json.RawMessage
}

// RecoveryGrant is a successful, single-use recovery claim returned only to the
// trusted hook handler. Its plaintext capability is never persisted.
type RecoveryGrant struct {
	Actor      string
	Capability string `json:"-"`
	StateDir   string
}

// LifecycleHost is the trusted host for adapters whose hooks own client state.
// It is bound to the hook's session and event. Policies run while core holds
// the journal lock: they must be fast and pure, without IO or Store access.
// Core enforces the bounds it can: copied inputs, validated and size-limited
// outputs, event-scoped transitions, and no write when a policy fails.
type LifecycleHost interface {
	HookHost
	ApplyLifecycle(func(model.SessionView) (LifecycleChange, error)) error
	ClaimRecovery(func(model.SessionView) (RecoveryDecision, error)) (RecoveryGrant, error)
}
