package bus

// Hooks receive lifecycle operations, never the service, credentials, registry,
// transport override, or arbitrary journal read/write access.
type HookHost interface {
	UpdateSession(string, SessionState, bool) error
	EmitPending(string) (string, error)
	CompletedNativePrompt(string, string) (string, error)
}

type HealthHost interface{ ListenerRunning(Session) bool }

type hookRuntime struct{ store Store }

func (h hookRuntime) UpdateSession(id string, state SessionState, retired bool) error {
	return h.store.Update(id, state, retired)
}
func (h hookRuntime) EmitPending(id string) (string, error) { return h.store.EmitPending(id) }
func (h hookRuntime) CompletedNativePrompt(id, prompt string) (string, error) {
	return h.store.completedNativePrompt(id, prompt)
}
