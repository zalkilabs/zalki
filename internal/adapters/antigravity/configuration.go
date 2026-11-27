package antigravity

import (
	"sort"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/clientconfig"
	"github.com/zalkilabs/zalki/internal/model"
)

// Paths of the two files, relative to the home directory. No environment
// variable moves ~/.gemini/config, so ConfigRootEnv stays empty.
const (
	hooksPath = ".gemini/config/hooks.json"
	mcpPath   = ".gemini/config/mcp_config.json"
)

// ownedEntry is the name of the hooks entry that is Zalki's. hooks.json is a map
// of named entries; other tools add their own and Antigravity runs all of them.
const ownedEntry = "zalki"

var _ agent.ConfigurationAdapter = Adapter{}

// Configuration declares the Antigravity hooks file and MCP file. The hooks file
// holds one named entry, "zalki", with one flat handler for each hook the
// descriptor declares; the installer owns that entry and no other key of the file.
// The MCP file uses the common mcpServers entry without a "type" key, which
// Antigravity's MCP config does not have.
func (a Adapter) Configuration(binary string, args []string) []model.ConfigDeclaration {
	command := clientconfig.HookCommand(binary, args, "antigravity")
	entry := map[string]any{}
	for event := range a.Descriptor().Hooks {
		entry[event] = []any{map[string]any{"type": "command", "command": command, "timeout": 5}}
	}
	return []model.ConfigDeclaration{
		{Path: hooksPath, Draft: "antigravity-hooks.json", JSON: map[string]any{ownedEntry: entry}, OwnedEntry: ownedEntry},
		{Path: mcpPath, Draft: "antigravity-mcp.json", JSON: clientconfig.MCPConfig(binary, args, false)},
	}
}

// hookEvents lists the hook events the adapter installs, in a fixed order.
func (a Adapter) hookEvents() []string {
	events := make([]string, 0, len(a.Descriptor().Hooks))
	for event := range a.Descriptor().Hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	return events
}
