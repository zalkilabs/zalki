package bus

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// writingHandler is an adapter whose handler writes its own reply for every event.
// It says whether the hook is complete through handled.
type writingHandler struct {
	stubCodexAdapter
	reply   string
	handled bool
}

func (h writingHandler) HandleHook(_ agent.HookHost, _ agent.HookInput, _ string, output io.Writer) (bool, error) {
	_, err := io.WriteString(output, h.reply)
	return h.handled, err
}

func hookWithHandler(t *testing.T, handler writingHandler) (stdout string, state model.SessionState) {
	t.Helper()
	registry, err := agent.NewRegistry(handler)
	if err != nil {
		t.Fatal(err)
	}
	store := Store{dir: filepath.Join(t.TempDir(), "state"), registry: registry}
	session, _ := register(t, store, "codex")
	input, err := json.Marshal(map[string]string{"hook_event_name": "PostToolUse", "session_id": session.NativeID, "cwd": "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	service := Service{Store: store}
	if err := service.Hook(t.Context(), "codex", bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	after, err := store.session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), after.State
}

// An adapter's handler may write the client's reply and return false: the bytes
// reach stdout unchanged, and core still applies the lifecycle for the event.
// This is how an adapter answers every hook with an object the client requires.
func TestHandlerOutputWithAnIncompleteHookKeepsTheLifecycle(t *testing.T) {
	out, state := hookWithHandler(t, writingHandler{reply: "{}", handled: false})
	if out != "{}" {
		t.Fatalf("stdout = %q, want the handler's bytes only", out)
	}
	if state != model.StateBusy {
		t.Fatalf("state = %q, want %q: core must still apply PostToolUse", state, model.StateBusy)
	}
}

// When the handler says it completed the hook, core leaves the session state to it.
func TestHandlerOutputWithACompleteHookSkipsTheLifecycle(t *testing.T) {
	out, state := hookWithHandler(t, writingHandler{reply: "{\"x\":1}\n", handled: true})
	if out != "{\"x\":1}\n" {
		t.Fatalf("stdout = %q, want the handler's bytes only", out)
	}
	if state == model.StateBusy {
		t.Fatalf("state = %q: core applied the lifecycle after a complete handler", state)
	}
}
