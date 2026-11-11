package cursor

import (
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
			got, err := Adapter{}.NormalizeHook(agent.HookInput{Event: tc.event, Status: tc.status})
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
		{"CLI shape", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"/fixture/w"}}, home, "C", "/fixture/w"},
		{"IDE start with both ids and no roots", agent.HookInput{SessionID: "S", ConversationID: "C", WorkspaceRoots: []string{}}, home, "C", home},
		{"IDE start with roots missing", agent.HookInput{SessionID: "S", ConversationID: "C"}, home, "C", home},
		{"session id only", agent.HookInput{SessionID: "S"}, home, "S", home},
		{"file URI is decoded", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"file:///fixture/proj%20dir"}}, home, "C", "/fixture/proj dir"},
		{"file URI with localhost", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"file://localhost/fixture/w"}}, home, "C", "/fixture/w"},
		{"file URI on another host is skipped", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"file://host/fixture/x", "/fixture/w"}}, home, "C", "/fixture/w"},
		{"remote scheme is skipped", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"vscode-remote://ssh-remote+h/fixture/y", "ssh://h/fixture/z", "/fixture/w"}}, home, "C", "/fixture/w"},
		{"other scheme without a host is skipped", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"vscode-vfs:///fixture/y", "/fixture/w"}}, home, "C", "/fixture/w"},
		{"control character in decoded path is skipped", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"file:///fixture/x%0Ay", "/fixture/w"}}, home, "C", "/fixture/w"},
		{"empty query mark is skipped", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"file:///fixture/a?", "/fixture/w"}}, home, "C", "/fixture/w"},
		{"relative file URI path is skipped", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"file:x", "/fixture/w"}}, home, "C", "/fixture/w"},
		{"relative root is skipped", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"rel", "/fixture/w"}}, home, "C", "/fixture/w"},
		{"only skipped roots fall back to HOME", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"vscode-remote://h/y"}}, home, "C", home},
		{"first local root wins", agent.HookInput{ConversationID: "C", WorkspaceRoots: []string{"/fixture/a", "file:///fixture/b"}}, home, "C", "/fixture/a"},
		{"absolute cwd wins", agent.HookInput{ConversationID: "C", CWD: "/fixture/c", WorkspaceRoots: []string{"/fixture/w"}}, home, "C", "/fixture/c"},
		{"relative cwd is ignored", agent.HookInput{ConversationID: "C", CWD: "rel", WorkspaceRoots: []string{"/fixture/w"}}, home, "C", "/fixture/w"},
		{"relative HOME is not used", agent.HookInput{ConversationID: "C"}, "rel", "C", ""},
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
