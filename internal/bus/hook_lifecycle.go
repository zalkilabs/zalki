package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// SessionView is the copy of a session that an adapter policy may inspect
// inside a lifecycle transaction. It carries no credential, and its adapter
// state is a fresh copy, so retaining or mutating it cannot reach the journal.
type SessionView = model.SessionView

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
	ApplyLifecycle(func(SessionView) (LifecycleChange, error)) error
	ClaimRecovery(func(SessionView) (RecoveryDecision, error)) (RecoveryGrant, error)
}

var allowedTransitions = map[string][]LifecycleTransition{
	"UserPromptSubmit": {TransitionNone, TransitionPromptReactivate},
	"Stop":             {TransitionNone, TransitionReady},
	"Interrupt":        {TransitionNone, TransitionReady},
	"PostToolUse":      {TransitionNone},
}

// Recovery reissues credentials, so only hooks that observed activity may claim it.
var recoveryEvents = map[string]bool{"PostToolUse": true, "Stop": true}

func viewOf(v Session) SessionView {
	return SessionView{Actor: v.ID, Kind: v.Kind, Generation: v.Generation, State: v.State, Retired: v.Retired,
		AdapterState: append(json.RawMessage(nil), v.AdapterState...)}
}

func transitionAllowed(event string, transition LifecycleTransition) bool {
	for _, allowed := range allowedTransitions[event] {
		if allowed == transition {
			return true
		}
	}
	return false
}

// applyAdapterState validates a policy's next state before it replaces the
// session's. The adapter's codec must accept it as a current-schema record,
// and whatever the codec returns is bounded and detached again before use.
func (s Store) applyAdapterState(v *Session, next json.RawMessage) error {
	if next == nil {
		return nil
	}
	candidate, err := boundedAdapterState(next)
	if err != nil {
		return err
	}
	final, err := s.decodeAdapterState(v.Kind, agent.StateInput{State: candidate})
	if err != nil {
		return err
	}
	v.AdapterState = final
	return nil
}

// lifecycleSession loads the hook's session for a transaction. An unknown
// session is not an error: startup must supply the initial identity.
func (s Store) lifecycleSession(actor, kind string) (Session, bool, error) {
	v, err := s.session(actor)
	if errors.Is(err, os.ErrNotExist) {
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	if v.Kind != kind {
		return v, false, fail(CodeInvalidIdentity, "lifecycle hook does not match the session's agent")
	}
	return v, true, nil
}

func (h hookRuntime) ApplyLifecycle(policy func(SessionView) (LifecycleChange, error)) error {
	return h.store.locked(func() error {
		v, ok, err := h.store.lifecycleSession(h.actor, h.kind)
		if !ok || err != nil {
			return err
		}
		change, err := policy(viewOf(v))
		if err != nil {
			return err
		}
		if !transitionAllowed(h.event, change.Transition) {
			return fail(CodeInvalidTransition, "lifecycle transition is not allowed for this hook event")
		}
		switch change.Transition {
		case TransitionPromptReactivate:
			v.State, v.UpdatedAt, v.Retired, v.RetiredReason = StateBusy, timestamp(), false, ""
		case TransitionReady:
			// As in Update, a stop never revives a retired conversation.
			if !v.Retired {
				v.State, v.UpdatedAt = StateReady, timestamp()
			}
		}
		if err := h.store.applyAdapterState(&v, change.AdapterState); err != nil {
			return err
		}
		return h.store.saveSession(v)
	})
}

// ClaimRecovery reissues the session's own capability when the adapter policy
// asks for it. Core first excludes retired, exited, approval-pending and
// actively listening sessions; the new credential, the policy's next state and
// the removal of any stale listener lease are persisted in one save. Actor,
// generation and pending messages are kept.
func (h hookRuntime) ClaimRecovery(policy func(SessionView) (RecoveryDecision, error)) (RecoveryGrant, error) {
	if !recoveryEvents[h.event] {
		return RecoveryGrant{}, fail(CodeInvalidTransition, "recovery is not allowed for this hook event")
	}
	var key string
	err := h.store.locked(func() error {
		v, ok, err := h.store.lifecycleSession(h.actor, h.kind)
		if !ok || err != nil {
			return err
		}
		if v.Retired || h.store.sessionLiveness(v) == LivenessExited || v.State == StateApproval || h.store.listenerStatus(v) == ListenerRunning {
			return nil
		}
		decision, err := policy(viewOf(v))
		if err != nil || !decision.Recover {
			return err
		}
		if err := h.store.applyAdapterState(&v, decision.AdapterState); err != nil {
			return err
		}
		// Only the hash is stored. Reissue this session's own capability rather
		// than recover plaintext from terminal logs or another session.
		next := newID() + newID()
		hash := sha256.Sum256([]byte(next))
		v.KeyHash = hex.EncodeToString(hash[:])
		// Every credential rotation retires the lease: a listener armed under
		// the old capability must not keep receiving for the new one.
		releaseListenerLease(&v)
		if err := h.store.saveSession(v); err != nil {
			return err
		}
		key = next
		return nil
	})
	if err != nil || key == "" {
		return RecoveryGrant{}, err
	}
	h.store.logEvent(slog.LevelInfo, "listener.recovery_requested", eventData{Actor: h.actor})
	return RecoveryGrant{Actor: h.actor, Capability: key, StateDir: h.store.Dir}, nil
}
