package bus

type HealthHost interface{ Listener(Session) ListenerStatus }

// hookRuntime is bound to one hook's session and lifecycle event; transaction
// operations act only on that session and only as that event allows.
type hookRuntime struct {
	store              Store
	actor, kind, event string
}

func (h hookRuntime) UpdateSession(id string, state SessionState, retired bool) error {
	return h.store.Update(id, state, retired)
}
func (h hookRuntime) EmitPending(id string) (string, error) { return h.store.EmitPending(id) }
func (h hookRuntime) CompletedNativePrompt(id, prompt string) (string, error) {
	return h.store.completedNativePrompt(id, prompt)
}

func (h hookRuntime) Listener(v Session) ListenerStatus { return h.store.listenerStatus(v) }
