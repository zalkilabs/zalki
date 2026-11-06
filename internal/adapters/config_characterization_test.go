package adapters_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
)

// TestBuiltinConfigurationDeclarations pins what the three built-in clients
// declare: the files, their draft names, the environment variable that
// relocates each file, the JSON patch structure, and exact shell/TOML text.
// Installer tests cover persisted formatting. A declaration-type refactor must
// preserve these values. Reading fields by name avoids coupling this test to
// the declaration type itself.
//
// The binary path and state directory contain a space and a quote on purpose,
// to pin how each client quotes them: shell quoting in hook commands, JSON in
// MCP entries and a TOML basic string in the Codex fragment.
func TestBuiltinConfigurationDeclarations(t *testing.T) {
	const binary, state = "/opt/my tools/it's zalki", "/var/my state"
	hookCommand := func(kind string) string {
		return `'/opt/my tools/it'"'"'s zalki' '--state' '/var/my state' 'hook' '--agent' '` + kind + `'`
	}
	// nested is the Claude and Codex hook shape; flat is Cursor's.
	nested := func(kind string, events ...string) string {
		command, _ := json.Marshal(hookCommand(kind))
		entries := make([]string, len(events))
		for i, event := range events {
			entries[i] = fmt.Sprintf(`%q:[{"hooks":[{"command":%s,"timeout":5,"type":"command"}]}]`, event, command)
		}
		return `{"hooks":{` + strings.Join(entries, ",") + `}}`
	}
	cursorCommand, _ := json.Marshal(hookCommand("cursor"))
	cursorHooks := fmt.Sprintf(`{"hooks":{`+
		`"beforeSubmitPrompt":[{"command":%[1]s,"timeout":5}],`+
		`"postToolUse":[{"command":%[1]s,"timeout":5}],`+
		`"sessionEnd":[{"command":%[1]s,"timeout":5}],`+
		`"sessionStart":[{"command":%[1]s,"timeout":5}],`+
		`"stop":[{"command":%[1]s,"loop_limit":5,"timeout":5}]},"version":1}`, cursorCommand)

	want := []struct {
		path, draft, root string
		json              string // Expected JSON patch, or "".
		toml              string // Expected TOML fragment, or "".
	}{
		{path: ".claude.json", draft: "claude-mcp.json", root: "CLAUDE_CONFIG_DIR",
			json: `{"mcpServers":{"zalki":{"args":["--state","/var/my state","mcp"],"command":"/opt/my tools/it's zalki","type":"stdio"}}}`},
		{path: ".claude/settings.json", draft: "claude-hooks.json", root: "CLAUDE_CONFIG_DIR",
			json: nested("claude", "PermissionRequest", "PostToolUse", "SessionEnd", "SessionStart", "Stop", "UserPromptSubmit")},
		{path: ".codex/config.toml", draft: "codex-mcp.toml", root: "CODEX_HOME",
			toml: "\n[mcp_servers.zalki]\ncommand = \"/opt/my tools/it's zalki\"\nargs = [\"--state\",\"/var/my state\",\"mcp\"]\n"},
		{path: ".codex/hooks.json", draft: "codex-hooks.json", root: "CODEX_HOME",
			json: nested("codex", "Interrupt", "PermissionRequest", "PostToolUse", "SessionEnd", "SessionStart", "Stop", "UserPromptSubmit")},
		{path: ".cursor/hooks.json", draft: "cursor-hooks.json", root: "", json: cursorHooks},
		{path: ".cursor/mcp.json", draft: "cursor-mcp.json", root: "",
			json: `{"mcpServers":{"zalki":{"args":["--state","/var/my state","mcp"],"command":"/opt/my tools/it's zalki"}}}`},
	}

	got, err := adapters.Builtin().Configuration(binary, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("Configuration returned %d files, want %d", len(got), len(want))
	}
	for i, w := range want {
		file := got[i]
		if file.Path != w.path {
			t.Fatalf("file %d: Path = %q, want %q (files are sorted by Path)", i, file.Path, w.path)
		}
		if file.Draft != w.draft {
			t.Errorf("%s: Draft = %q, want %q", w.path, file.Draft, w.draft)
		}
		if file.ConfigRootEnv != w.root {
			t.Errorf("%s: ConfigRootEnv = %q, want %q", w.path, file.ConfigRootEnv, w.root)
		}
		if file.TOML != w.toml {
			t.Errorf("%s: TOML = %q, want %q", w.path, file.TOML, w.toml)
		}
		if (file.JSON == nil) != (w.json == "") {
			t.Errorf("%s: JSON is %v, want a patch: %v", w.path, file.JSON != nil, w.json != "")
			continue
		}
		if w.json == "" {
			continue
		}
		// Compare as decoded JSON so number types and key order do not matter,
		// and the file is written the way the installer would encode it.
		encoded, err := json.Marshal(file.JSON)
		if err != nil {
			t.Fatalf("%s: JSON cannot be encoded: %v", w.path, err)
		}
		var gotValue, wantValue any
		if err := json.Unmarshal(encoded, &gotValue); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(w.json), &wantValue); err != nil {
			t.Fatalf("%s: bad expectation: %v", w.path, err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Errorf("%s: JSON patch differs\n got %s\nwant %s", w.path, encoded, w.json)
		}
	}
}

// TestBuiltinConfigurationIsIndependentPerCall pins that every call builds
// fresh values: changing one result does not change what the next call returns.
func TestBuiltinConfigurationIsIndependentPerCall(t *testing.T) {
	registry := adapters.Builtin()
	encode := func(label string) string {
		files, err := registry.Configuration("/bin/zalki", "/state")
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		type view struct {
			Path, Draft, Root, TOML string
			JSON                    map[string]any
		}
		views := make([]view, 0, len(files))
		for _, file := range files {
			views = append(views, view{file.Path, file.Draft, file.ConfigRootEnv, file.TOML, file.JSON})
		}
		b, err := json.Marshal(views)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	reference := encode("reference")

	files, err := registry.Configuration("/bin/zalki", "/state")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		for key := range file.JSON {
			delete(file.JSON, key)
		}
		if file.JSON != nil {
			file.JSON["injected"] = true
		}
	}
	files[0].Path = "changed"
	if again := encode("after a caller changed a result"); again != reference {
		t.Fatalf("a caller's changes reached a later call\n got %s\nwant %s", again, reference)
	}
}
