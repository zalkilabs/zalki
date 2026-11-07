package bus

import "github.com/zalkilabs/zalki/internal/model"

// hookRuntime supplies trusted hook and diagnostic host operations.
// UpdateSession, EmitPending and CompletedNativePrompt require its nonempty
// actor and reject other ids before delegating. ApplyLifecycle and ClaimRecovery
// keep their actor, kind and event checks. Listener is independent health
// evidence and does not require an actor binding.
type hookRuntime struct {
	store              Store
	actor, kind, event string
}

func (h hookRuntime) UpdateSession(id string, state SessionState, retired bool) error {
	if h.actor == "" || id != h.actor {
		return model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	return h.store.Update(id, state, retired)
}
func (h hookRuntime) EmitPending(id string) (string, error) {
	if h.actor == "" || id != h.actor {
		return "", model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	return h.store.EmitPending(id)
}
func (h hookRuntime) CompletedNativePrompt(id, prompt string) (string, error) {
	if h.actor == "" || id != h.actor {
		return "", model.NewError(model.CodeUnauthorized, "hook operation must use its bound session")
	}
	return h.store.completedNativePrompt(id, prompt)
}

func (h hookRuntime) Listener(v Session) ListenerStatus { return h.store.listenerStatus(v) }
