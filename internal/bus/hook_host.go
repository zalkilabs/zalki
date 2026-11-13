package bus

import (
	"errors"
	"os"
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

var errStaleHook = errors.New("hook registration changed")

// hookRuntime supplies trusted hook and diagnostic host operations.
// UpdateSession, EmitPending and CompletedNativePrompt require its nonempty
// actor and reject other ids before delegating. ApplyLifecycle and ClaimRecovery
// keep their actor, kind and event checks. Listener is independent health
// evidence and does not require an actor binding.
type hookRuntime struct {
	store              Store
	actor, kind, event string
	generation         string
	generationBound    bool
}

func (h hookRuntime) UpdateSession(id string, state model.SessionState, retired bool) error {
	if h.actor == "" || id != h.actor {
		return model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	return updateSession(h.store, id, state, retired, lifecycleCause(h.event), h)
}
func (h hookRuntime) EmitPending(id string) (string, error) {
	if h.actor == "" || id != h.actor {
		return "", model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	var context strings.Builder
	err := h.store.locked(func() error {
		if _, err := sessionForHook(h.store, id, h); err != nil {
			return err
		}
		return h.store.emitPendingLocked(id, &context)
	})
	return context.String(), err
}
func (h hookRuntime) CompletedNativePrompt(id, prompt string) (string, error) {
	if h.actor == "" || id != h.actor {
		return "", model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	return h.store.completedNativePrompt(id, prompt)
}

func (h hookRuntime) Listener(v model.Session) model.ListenerStatus { return h.store.listenerStatus(v) }

// Called only inside the operation's existing journal lock. A bound missing
// snapshot never adopts a registration created by another hook later.
func sessionForHook(s Store, actor string, snapshots ...hookRuntime) (model.Session, error) {
	v, err := s.session(actor)
	if len(snapshots) == 0 || !snapshots[0].generationBound {
		return v, err
	}
	h := snapshots[0]
	if errors.Is(err, os.ErrNotExist) {
		return v, errStaleHook
	}
	if err != nil {
		return v, err
	}
	if h.actor != actor || v.ID != actor || v.Kind != h.kind || h.generation == "" || v.Generation != h.generation {
		return v, errStaleHook
	}
	return v, nil
}

func lifecycleSessionForHook(h hookRuntime) (model.Session, bool, error) {
	if !h.generationBound {
		return h.store.lifecycleSession(h.actor, h.kind)
	}
	v, err := sessionForHook(h.store, h.actor, h)
	if errors.Is(err, os.ErrNotExist) {
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	if v.Kind != h.kind {
		return v, false, model.NewError(model.CodeInvalidIdentity, "lifecycle hook does not match the session's agent")
	}
	return v, true, nil
}
