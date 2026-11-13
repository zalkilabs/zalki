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

var allowedTransitions = map[string][]agent.LifecycleTransition{
	"UserPromptSubmit": {agent.TransitionNone, agent.TransitionPromptReactivate},
	"Stop":             {agent.TransitionNone, agent.TransitionReady},
	"Interrupt":        {agent.TransitionNone, agent.TransitionReady},
	"PostToolUse":      {agent.TransitionNone},
}

// Recovery reissues credentials, so only hooks that observed activity may claim it.
var recoveryEvents = map[string]bool{"PostToolUse": true, "Stop": true}

func viewOf(v model.Session) model.SessionView {
	return model.SessionView{Actor: v.ID, Kind: v.Kind, Generation: v.Generation, State: v.State, Retired: v.Retired,
		AdapterState: append(json.RawMessage(nil), v.AdapterState...)}
}

func transitionAllowed(event string, transition agent.LifecycleTransition) bool {
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
func (s Store) applyAdapterState(v *model.Session, next json.RawMessage) error {
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
func (s Store) lifecycleSession(actor, kind string) (model.Session, bool, error) {
	v, err := s.session(actor)
	if errors.Is(err, os.ErrNotExist) {
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	if v.Kind != kind {
		return v, false, model.NewError(model.CodeInvalidIdentity, "lifecycle hook does not match the session's agent")
	}
	return v, true, nil
}

func (h hookRuntime) ApplyLifecycle(policy func(model.SessionView) (agent.LifecycleChange, error)) error {
	return h.store.locked(func() error {
		v, ok, err := lifecycleSessionForHook(h)
		if !ok || err != nil {
			return err
		}
		change, err := policy(viewOf(v))
		if err != nil {
			return err
		}
		if !transitionAllowed(h.event, change.Transition) {
			return model.NewError(model.CodeInvalidTransition, "lifecycle transition is not allowed for this hook event")
		}
		switch change.Transition {
		case agent.TransitionPromptReactivate:
			v.State, v.UpdatedAt, v.Retired, v.RetiredReason = model.StateBusy, model.Timestamp(), false, ""
		case agent.TransitionReady:
			// As in Update, a stop never revives a retired conversation.
			if !v.Retired {
				v.State, v.UpdatedAt = model.StateReady, model.Timestamp()
			}
		}
		if err := h.store.applyAdapterState(&v, change.AdapterState); err != nil {
			return err
		}
		return saveSessionFor(h.store, v, lifecycleCause(h.event))
	})
}

// ClaimRecovery reissues the session's own capability when the adapter policy
// asks for it. Core first excludes retired, exited, approval-pending and
// actively listening sessions; the new credential, the policy's next state and
// the removal of any stale listener lease are persisted in one save. Actor,
// generation and pending messages are kept.
func (h hookRuntime) ClaimRecovery(policy func(model.SessionView) (agent.RecoveryDecision, error)) (agent.RecoveryGrant, error) {
	if !recoveryEvents[h.event] {
		return agent.RecoveryGrant{}, model.NewError(model.CodeInvalidTransition, "recovery is not allowed for this hook event")
	}
	var key string
	err := h.store.locked(func() error {
		v, ok, err := lifecycleSessionForHook(h)
		if !ok || err != nil {
			return err
		}
		if v.Retired || h.store.sessionLiveness(v) == model.LivenessExited || v.State == model.StateApproval || h.store.listenerStatus(v) == model.ListenerRunning {
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
		if err := saveSessionFor(h.store, v, model.CauseRecovery); err != nil {
			return err
		}
		key = next
		return nil
	})
	if err != nil || key == "" {
		return agent.RecoveryGrant{}, err
	}
	h.store.logEvent(slog.LevelInfo, "listener.recovery_requested", eventData{Actor: h.actor})
	return agent.RecoveryGrant{Instructions: startupInstructions(h.actor, key), Actor: h.actor, Capability: key, StateDir: h.store.Dir, ConfigFile: h.store.ConfigFile}, nil
}
