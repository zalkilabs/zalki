package bus

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// replyAdapter is a client with a prompt hook that may register a session. Its
// descriptor declares the reply (empty: none) and its handler may write its own.
type replyAdapter struct {
	stubCursorAdapter
	reply string
	// write is what the handler writes on a prompt hook, before it completes.
	write string
}

func (a replyAdapter) Descriptor() agent.AdapterDescriptor {
	d := a.stubCursorAdapter.Descriptor()
	d.Lifecycle.PromptReply = a.reply
	return d
}

func (a replyAdapter) HandleHook(host agent.HookHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	if event.Event == "UserPromptSubmit" && a.write != "" {
		_, err := io.WriteString(output, a.write)
		return false, err
	}
	return a.stubCursorAdapter.HandleHook(host, event, id, output)
}

func promptHook(t *testing.T, adapter replyAdapter, native string) string {
	t.Helper()
	registry, err := agent.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: Store{dir: filepath.Join(t.TempDir(), "state"), registry: registry}}
	input, err := json.Marshal(map[string]any{"conversation_id": newID(), "hook_event_name": native, "cwd": "/workspace", "workspace_roots": []string{"/workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := service.Hook(t.Context(), "cursor", bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Core copies the reply the adapter declared and adds one newline. It has no
// reply of its own: this client's reply is not any client's real one.
func TestCoreWritesTheReplyTheAdapterDeclaredAfterAPromptHook(t *testing.T) {
	if got := promptHook(t, replyAdapter{reply: `{"declared":["by","the adapter"]}`}, "beforeSubmitPrompt"); got != "{\"declared\":[\"by\",\"the adapter\"]}\n" {
		t.Fatalf("stdout = %q", got)
	}
}

// An adapter that may register sessions but declares no reply gets none.
func TestCoreWritesNothingAfterAPromptHookWithoutADeclaredReply(t *testing.T) {
	if got := promptHook(t, replyAdapter{}, "beforeSubmitPrompt"); got != "" {
		t.Fatalf("stdout = %q, want nothing", got)
	}
}

// The reply is only for the prompt hook that may register: not for other events.
func TestCoreWritesNoDeclaredReplyAfterOtherEvents(t *testing.T) {
	for _, native := range []string{"postToolUse", "sessionEnd"} {
		if got := promptHook(t, replyAdapter{reply: `{"declared":1}`}, native); got != "" {
			t.Fatalf("%s: stdout = %q, want nothing", native, got)
		}
	}
}

// When the adapter's handler wrote its own reply, core adds nothing: stdout
// never holds two replies.
func TestCoreWritesNoDeclaredReplyAfterTheAdapterWroteOne(t *testing.T) {
	if got := promptHook(t, replyAdapter{reply: `{"declared":1}`, write: "{\"own\":true}\n"}, "beforeSubmitPrompt"); got != "{\"own\":true}\n" {
		t.Fatalf("stdout = %q, want only the adapter's bytes", got)
	}
}
