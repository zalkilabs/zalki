package bus

import (
	"errors"
	"os"
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

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
	pluginStartup      pluginStartupFormat
}

func (h hookRuntime) UpdateSession(id string, state model.SessionState, retired bool) error {
	if h.actor == "" || id != h.actor {
		return model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	return updateSession(h.store, id, state, retired, lifecycleCause(h.event), fenceOf(h))
}
func (h hookRuntime) EmitPending(id string) (string, error) {
	if h.actor == "" || id != h.actor {
		return "", model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	var context strings.Builder
	err := h.store.locked(func() error {
		if _, err := sessionUnderFence(h.store, id, fenceOf(h)); err != nil {
			return err
		}
		return inboxReads(h.store).emitPendingLocked(id, &context)
	})
	return context.String(), err
}
func (h hookRuntime) CompletedNativePrompt(id, prompt string) (string, error) {
	if h.actor == "" || id != h.actor {
		return "", model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	return h.store.completedNativePrompt(id, prompt)
}

func (h hookRuntime) Listener(v model.Session) model.ListenerStatus {
	return listeners(h.store).listenerStatus(v)
}

func lifecycleSessionForHook(h hookRuntime) (model.Session, bool, error) {
	if !h.generationBound {
		return h.store.lifecycleSession(h.actor, h.kind)
	}
	v, err := sessionUnderFence(h.store, h.actor, fenceOf(h))
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
