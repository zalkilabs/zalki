package cursor

import (
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// Core decides completion from canonical events, so the adapter must turn each
// native Stop status into a canonical event.
func TestNormalizeHookMapsNativeStopStatusToCanonicalEvents(t *testing.T) {
	for _, tc := range []struct {
		name, event, status, want string
	}{
		{"completed", "Stop", "completed", "Stop"},
		{"no status", "Stop", "", "Stop"},
		{"aborted", "Stop", "aborted", "Interrupt"},
		{"error", "Stop", "error", "Interrupt"},
		{"unknown status", "Stop", "something-new", "Interrupt"},
		{"other event ignores status", "PostToolUse", "aborted", "PostToolUse"},
		{"session end ignores status", "SessionEnd", "aborted", "SessionEnd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Adapter{}.NormalizeHook(withNative(agent.HookInput{Event: tc.event}, map[string]any{"status": tc.status}))
			if err != nil || got.Event != tc.want {
				t.Fatalf("event %q status %q: got %q, %v; want %q", tc.event, tc.status, got.Event, err, tc.want)
			}
		})
	}
}

// The IDE sends session_id only on sessionStart and sessionEnd, and may send no
// cwd and no usable root. These payloads follow Cursor's hook docs; they are not
// a recorded capture.
func TestNormalizeHookIDEShapes(t *testing.T) {
	const home = "/fixture/home"
	for _, tc := range []struct {
		name                  string
		in                    agent.HookInput
		home, wantID, wantCWD string
	}{
		{"CLI shape", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"/fixture/w"}}), home, "C", "/fixture/w"},
		{"IDE start with both ids and no roots", withNative(agent.HookInput{SessionID: "S"}, map[string]any{"conversation_id": "C", "workspace_roots": []string{}}), home, "C", home},
		{"IDE start with roots missing", withNative(agent.HookInput{SessionID: "S"}, map[string]any{"conversation_id": "C"}), home, "C", home},
		{"session id only", agent.HookInput{SessionID: "S"}, home, "S", home},
		{"file URI is decoded", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"file:///fixture/proj%20dir"}}), home, "C", "/fixture/proj dir"},
		{"file URI with localhost", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"file://localhost/fixture/w"}}), home, "C", "/fixture/w"},
		{"file URI on another host is skipped", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"file://host/fixture/x", "/fixture/w"}}), home, "C", "/fixture/w"},
		{"remote scheme is skipped", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"vscode-remote://ssh-remote+h/fixture/y", "ssh://h/fixture/z", "/fixture/w"}}), home, "C", "/fixture/w"},
		{"other scheme without a host is skipped", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"vscode-vfs:///fixture/y", "/fixture/w"}}), home, "C", "/fixture/w"},
		{"control character in decoded path is skipped", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"file:///fixture/x%0Ay", "/fixture/w"}}), home, "C", "/fixture/w"},
		{"empty query mark is skipped", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"file:///fixture/a?", "/fixture/w"}}), home, "C", "/fixture/w"},
		{"relative file URI path is skipped", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"file:x", "/fixture/w"}}), home, "C", "/fixture/w"},
		{"relative root is skipped", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"rel", "/fixture/w"}}), home, "C", "/fixture/w"},
		{"only skipped roots fall back to HOME", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"vscode-remote://h/y"}}), home, "C", home},
		{"first local root wins", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"/fixture/a", "file:///fixture/b"}}), home, "C", "/fixture/a"},
		{"absolute cwd wins", withNative(agent.HookInput{CWD: "/fixture/c"}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"/fixture/w"}}), home, "C", "/fixture/c"},
		{"relative cwd is ignored", withNative(agent.HookInput{CWD: "rel"}, map[string]any{"conversation_id": "C", "workspace_roots": []string{"/fixture/w"}}), home, "C", "/fixture/w"},
		{"relative HOME is not used", withNative(agent.HookInput{}, map[string]any{"conversation_id": "C"}), "rel", "C", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", tc.home)
			got, err := Adapter{}.NormalizeHook(tc.in)
			if err != nil || got.SessionID != tc.wantID || got.CWD != tc.wantCWD {
				t.Fatalf("got id %q cwd %q, %v; want id %q cwd %q", got.SessionID, got.CWD, err, tc.wantID, tc.wantCWD)
			}
		})
	}
}

// withNative sets the Cursor fields core would keep in HookInput.Native.
func withNative(in agent.HookInput, native map[string]any) agent.HookInput {
	raw, err := json.Marshal(native)
	if err != nil {
		panic(err)
	}
	in.Native = raw
	return in
}
