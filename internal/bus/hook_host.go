package bus

// HookHost exposes bounded lifecycle operations, never the service, registry,
// transport override, or arbitrary journal read/write access. The trusted Cursor
// runtime additionally supports a receiver-owned recovery claim, not arbitrary
// credential reads or rotation.
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

// CursorRecovery is a successful, single-use listener recovery claim returned
// only to the trusted hook handler. Its plaintext capability is never persisted.
type CursorRecovery struct {
	Actor      string
	Capability string `json:"-"`
	StateDir   string
}

func (h hookRuntime) CursorPrompt(id string) error { return h.store.cursorPrompt(id) }
func (h hookRuntime) SuppressCursorRecovery(id string) error {
	return h.store.suppressCursorRecovery(id)
}
func (h hookRuntime) ClaimCursorRecovery(id string) (CursorRecovery, error) {
	return h.store.claimCursorRecovery(id)
}
func (h hookRuntime) ListenerRunning(v Session) bool { return h.store.cursorListenerRunning(v) }
