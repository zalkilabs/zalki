package install_test

import (
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

func fixtureConfiguration(t *testing.T, binary, state string) (map[string]map[string]any, string) {
	t.Helper()
	files, err := bus.Configuration(binary, state)
	if err != nil {
		t.Fatal(err)
	}
	patches := map[string]map[string]any{}
	fragment := ""
	for _, file := range files {
		if file.JSON != nil {
			patches[file.Path] = file.JSON
		} else {
			fragment = file.TOML
		}
	}
	return patches, fragment
}

func TestCodexFragmentOwnershipRejectsExtraTables(t *testing.T) {
	clearConfigOverrides(t)
	_, fragment := fixtureConfiguration(t, "/bin/zalki", "/state")
	for _, extra := range []string{"\n[unrelated]\nvalue = true\n", "\n[mcp_servers.other]\ncommand = 'keep'\n"} {
		if _, err := install.RemoveMCPFragment([]byte(fragment+extra), fragment+extra); err == nil {
			t.Fatal("accepted ownership of extra recorded TOML tables")
		}
	}
	var patch map[string]any
	if err := json.Unmarshal([]byte(`{"mcpServers":{"zalki":{},"other":{}}}`), &patch); err != nil {
		t.Fatal(err)
	}
	if err := install.ValidateInstallationPatch(patch); err == nil {
		t.Fatal("accepted ambiguous MCP ownership")
	}
}
