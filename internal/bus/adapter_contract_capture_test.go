package bus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestContractCaptureExcludesPrivateValues(t *testing.T) {
	root := filepath.Join(t.TempDir(), "contracts")
	t.Setenv("ZALKI_CAPTURE_CONTRACTS", root)
	t.Setenv("PATH", t.TempDir())
	title := "private project title"
	const identity = "private native identity"
	native := json.RawMessage(`{"conversation_id":"` + identity + `","workspace_roots":["/private/workstation/project"],"status":"private status","loop_count":3}`)
	event := agent.HookInput{Event: "sessionStart", Native: native, CWD: "/private/workstation/project", Prompt: "private prompt and credential", SessionTitle: &title}
	if err := captureHookContract(t.Context(), stubCursorAdapter{}, event); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cursor-unknown.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{identity, "private status", event.CWD, event.Prompt, title} {
		if strings.Contains(string(raw), private) {
			t.Fatal("capture leaked private values")
		}
	}
	if !strings.Contains(string(raw), "/fixture/workspace") || !strings.Contains(string(raw), "conversation_id") {
		t.Fatal("capture lost the native schema shape")
	}
	if err := captureHookContract(t.Context(), stubCursorAdapter{}, event); err != nil {
		t.Fatal("capture should preserve an existing fixture", err)
	}
}
