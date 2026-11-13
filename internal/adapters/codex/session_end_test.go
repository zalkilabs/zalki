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

func TestCodexSessionEndBoundUpdateAndError(t *testing.T) {
	failure := errors.New("controlled write failure")
	for _, want := range []error{nil, failure} {
		host := &endHookHost{err: want}
		handled, err := New(false, "fixture.json").HandleHook(host, agent.HookInput{Event: "SessionEnd"}, "codex:fixture", io.Discard)
		if !handled || host.calls != 1 || host.id != "codex:fixture" || host.state != model.StateReady || host.retired {
			t.Fatal("ZAL34_CODEX_END_BOUND_UPDATE_CHANGED")
		}
		if !errors.Is(err, want) {
			t.Fatal("ZAL34_CODEX_END_DROPPED_WRITE_ERROR")
		}
	}
}
