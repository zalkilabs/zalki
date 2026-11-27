package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

var configurationArgs = []string{"--config", "/fixture/config.json", "--state", "/fixture/state dir"}

const configurationBinary = "/opt/zalki tools/zalki"

func TestConfigurationDeclaresTheHooksEntryAndTheMCPServer(t *testing.T) {
	files := New().Configuration(configurationBinary, configurationArgs)
	if len(files) != 2 {
		t.Fatalf("declared %d files, want 2", len(files))
	}
	hooks, mcp := files[0], files[1]
	if hooks.Path != ".gemini/config/hooks.json" || hooks.Draft != "antigravity-hooks.json" || hooks.OwnedEntry != "zalki" || hooks.ConfigRootEnv != "" || hooks.TOML != "" {
		t.Fatalf("hooks declaration = %+v", hooks)
	}
	if mcp.Path != ".gemini/config/mcp_config.json" || mcp.Draft != "antigravity-mcp.json" || mcp.OwnedEntry != "" || mcp.ConfigRootEnv != "" || mcp.TOML != "" {
		t.Fatalf("MCP declaration = %+v", mcp)
	}
	requireGolden(t, "hooks", hooks.JSON, "configuration-hooks.json")
	requireGolden(t, "mcp", mcp.JSON, "configuration-mcp.json")
}

func requireGolden(t *testing.T, name string, got any, golden string) {
	t.Helper()
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", golden))
	if err != nil || string(raw)+"\n" != string(want) {
		t.Fatalf("%s patch differs from testdata/%s (%v)\ngot:\n%s\nwant:\n%s", name, golden, err, raw, want)
	}
}

// The hooks entry holds one flat handler for each hook the descriptor declares, and
// nothing that belongs to another client (Cursor's loop limit, a matcher group).
func TestConfigurationInstallsExactlyTheDeclaredHooks(t *testing.T) {
	entry := New().Configuration(configurationBinary, configurationArgs)[0].JSON["zalki"].(map[string]any)
	events := make([]string, 0, len(entry))
	for event, value := range entry {
		events = append(events, event)
		handlers := value.([]any)
		if len(handlers) != 1 {
			t.Fatalf("%s has %d handlers, want 1", event, len(handlers))
		}
		handler := handlers[0].(map[string]any)
		command, _ := handler["command"].(string)
		if len(handler) != 3 || handler["type"] != "command" || handler["timeout"] != 5 || !strings.Contains(command, "'hook' '--agent' 'antigravity'") {
			t.Fatalf("%s handler = %v", event, handler)
		}
	}
	declared := make([]string, 0, len(entry))
	for event := range New().Descriptor().Hooks {
		declared = append(declared, event)
	}
	sort.Strings(events)
	sort.Strings(declared)
	if !reflect.DeepEqual(events, declared) || !reflect.DeepEqual(events, []string{"PreInvocation", "SessionStart", "Stop"}) {
		t.Fatalf("installed events %v, declared %v; want PreInvocation, SessionStart, Stop (no PostInvocation, no tool hook)", events, declared)
	}
}

func TestConfigurationPassesTheRegistryChecks(t *testing.T) {
	registry, err := agent.NewRegistry(New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Configuration(configurationBinary, configurationArgs); err != nil {
		t.Fatalf("the registry refuses the declaration: %v", err)
	}
}
