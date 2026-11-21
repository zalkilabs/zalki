package codex

import (
	"errors"
	"io"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type endHookHost struct {
	id      string
	state   model.SessionState
	retired bool
	calls   int
	err     error
}

func (h *endHookHost) UpdateSession(id string, state model.SessionState, retired bool) error {
	h.id, h.state, h.retired = id, state, retired
	h.calls++
	return h.err
}
func (*endHookHost) EmitPending(string) (string, error) {
	return "", errors.New("unexpected pending read")
}
func (*endHookHost) CompletedNativePrompt(string, string) (string, error) {
	return "", errors.New("unexpected prompt read")
}

// Core keeps an ended Codex session from its descriptor; the hook itself writes nothing.
func TestCodexSessionEndIsCoreOwned(t *testing.T) {
	adapter := New(false, "fixture.json")
	if !adapter.Descriptor().Lifecycle.KeepOnSessionEnd {
		t.Fatal("CODEX_END_NOT_KEPT_BY_DESCRIPTOR")
	}
	host := &endHookHost{}
	handled, err := adapter.HandleHook(host, agent.HookInput{Event: "SessionEnd"}, "codex:fixture", io.Discard)
	if handled || err != nil || host.calls != 0 {
		t.Fatal("CODEX_END_HOOK_WROTE_STATE")
	}
}
