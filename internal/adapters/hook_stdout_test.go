package adapters_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// These tests pin what each client's hook writes to stdout today, byte for byte
// where the bytes do not depend on a random id. A change of who writes a reply
// (core or the adapter) must not change one of them.

// hookStdout runs one hook of kind with the payload and returns stdout and the error.
func hookStdout(t *testing.T, service bus.Service, kind string, payload map[string]any) (string, error) {
	t.Helper()
	input, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = service.Hook(t.Context(), kind, bytes.NewReader(input), &out)
	return out.String(), err
}

func isolatedClientRoots(t *testing.T) {
	t.Helper()
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, t.TempDir())
	}
}

// hookPayload is a payload for the native event of a registered or unknown session.
func hookPayload(kind, native, event string) map[string]any {
	payload := map[string]any{"hook_event_name": event, "cwd": "/workspace", "prompt": "hello"}
	if kind == "cursor" {
		payload["conversation_id"] = native
		payload["status"] = "completed"
		payload["workspace_roots"] = []string{"/workspace"}
	} else {
		payload["session_id"] = native
	}
	return payload
}

// Claude and Codex write nothing for any event but SessionStart, whether the
// session is known or not.
func TestClaudeAndCodexHooksWriteNothingAfterSessionStart(t *testing.T) {
	isolatedClientRoots(t)
	events := map[string][]string{
		"claude": {"SessionEnd", "PermissionRequest", "PostToolUse", "UserPromptSubmit", "Stop"},
		"codex":  {"SessionEnd", "PermissionRequest", "PostToolUse", "UserPromptSubmit", "Stop", "Interrupt"},
	}
	for _, kind := range []string{"claude", "codex"} {
		for _, event := range events[kind] {
			store := adapterTestStore(t)
			service := bus.Service{Store: store}
			session, _ := registerAdapterSession(t, store, kind)
			for name, native := range map[string]string{"known session": session.NativeID, "unknown session": model.NewUUID()} {
				out, err := hookStdout(t, service, kind, hookPayload(kind, native, event))
				if err != nil || out != "" {
					t.Fatalf("%s %s (%s): stdout %q, error %v; want nothing", kind, event, name, out, err)
				}
			}
		}
	}
}

// Cursor's prompt hook answers with exactly one line, whether the chat is known
// or new, and Cursor writes nothing on a session end or for a chat core does not know.
func TestCursorHookStdoutForPromptAndSessionEnd(t *testing.T) {
	isolatedClientRoots(t)
	store := adapterTestStore(t)
	service := bus.Service{Store: store}
	known, _ := registerAdapterSession(t, store, "cursor")
	const reply = "{\"continue\":true}\n"
	for name, test := range map[string]struct {
		native, event, want string
	}{
		"prompt of a known chat":    {known.NativeID, "beforeSubmitPrompt", reply},
		"prompt of a new chat":      {model.NewUUID(), "beforeSubmitPrompt", reply},
		"session end":               {known.NativeID, "sessionEnd", ""},
		"tool of an unknown chat":   {model.NewUUID(), "postToolUse", ""},
		"stop of an unknown chat":   {model.NewUUID(), "stop", ""},
		"session end, unknown chat": {model.NewUUID(), "sessionEnd", ""},
	} {
		out, err := hookStdout(t, service, "cursor", hookPayload("cursor", test.native, test.event))
		if err != nil || out != test.want {
			t.Fatalf("%s: stdout %q, error %v; want %q", name, out, err, test.want)
		}
	}
}

// When registering the chat fails, the prompt is still not blocked: the same
// single line goes to stdout, and the reason goes to stderr.
func TestCursorPromptReplyIsTheSameLineWhenRegistrationFails(t *testing.T) {
	isolatedClientRoots(t)
	store := adapterTestStore(t)
	registerAdapterSession(t, store, "codex") // Create a private state directory.
	if err := os.WriteFile(filepath.Join(store.Dir(), "hook-registration-quota.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	var out string
	var err error
	stderr, captureErr := captureCursorStderr(t, func() error {
		out, err = hookStdout(t, bus.Service{Store: store}, "cursor", hookPayload("cursor", model.NewUUID(), "beforeSubmitPrompt"))
		return err
	})
	if captureErr != nil || out != "{\"continue\":true}\n" || !strings.Contains(stderr, "Your message was not blocked.") {
		t.Fatalf("stdout %q, stderr %q, error %v", out, stderr, captureErr)
	}
}

// Cursor's context replies are one JSON object on one line with one key: the
// text goes in additional_context after a tool, and in followup_message after a stop.
func TestCursorContextReplyIsOneObjectWithOneKey(t *testing.T) {
	isolatedClientRoots(t)
	for event, key := range map[string]string{"postToolUse": "additional_context", "stop": "followup_message"} {
		store := adapterTestStore(t)
		service := bus.Service{Store: store}
		known, _ := registerAdapterSession(t, store, "cursor")
		out, err := hookStdout(t, service, "cursor", hookPayload("cursor", known.NativeID, event))
		var object map[string]any
		if err != nil || !strings.HasSuffix(out, "}\n") || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &object) != nil {
			t.Fatalf("%s: stdout is not one JSON object on one line: %q (%v)", event, out, err)
		}
		keys := make([]string, 0, len(object))
		for k := range object {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) != 1 || keys[0] != key {
			t.Fatalf("%s: keys %v, want only %q", event, keys, key)
		}
	}
}

// A hook that cannot be decoded, or that names an event the client does not
// have, writes nothing and fails.
func TestUndecodableHooksWriteNothingAndFail(t *testing.T) {
	isolatedClientRoots(t)
	for _, kind := range []string{"claude", "codex", "cursor"} {
		service := bus.Service{Store: adapterTestStore(t)}
		var out bytes.Buffer
		if err := service.Hook(t.Context(), kind, strings.NewReader("{"), &out); err == nil || out.Len() != 0 {
			t.Fatalf("%s: broken JSON: stdout %q, error %v", kind, out.String(), err)
		}
		out.Reset()
		unknown := map[string]any{"hook_event_name": "NoSuchEvent", "session_id": model.NewUUID(), "conversation_id": model.NewUUID(), "cwd": "/workspace"}
		if got, err := hookStdout(t, service, kind, unknown); err == nil || got != "" {
			t.Fatalf("%s: unknown event: stdout %q, error %v", kind, got, err)
		}
	}
}
