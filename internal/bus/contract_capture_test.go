package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractCaptureExcludesPrivateValues(t *testing.T) {
	root := filepath.Join(t.TempDir(), "contracts")
	t.Setenv("ZALKI_CAPTURE_CONTRACTS", root)
	t.Setenv("PATH", t.TempDir())
	title := "private project title"
	event := HookInput{Event: "sessionStart", ConversationID: "private native identity", CWD: "/private/workstation/project", WorkspaceRoots: []string{"/private/workstation/project"}, Prompt: "private prompt and credential", SessionTitle: &title}
	if err := captureHookContract(stubCursorAdapter{}, event); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cursor-unknown.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{event.ConversationID, event.CWD, event.Prompt, title} {
		if strings.Contains(string(raw), private) {
			t.Fatal("capture leaked private values")
		}
	}
	if !strings.Contains(string(raw), "/fixture/workspace") || !strings.Contains(string(raw), "conversation_id") {
		t.Fatal("capture lost the native schema shape")
	}
	if err := captureHookContract(stubCursorAdapter{}, event); err != nil {
		t.Fatal("capture should preserve an existing fixture", err)
	}
}
