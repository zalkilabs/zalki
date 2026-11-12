package install_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests pin how upgrade and uninstall take zalki's entries out of
// settings files the user may have changed since install: what is refused,
// with which exact message, and what is tolerated. A refusal must leave every
// file, including the installation record, untouched.
// Exact error text and inconsistent prefixes are characterized, not endorsed;
// changing them is a deliberate behavior change.

// installedHome installs the built-in clients into a fresh home and returns it
// with its state directory.
func installedHome(t *testing.T) (home, state string) {
	t.Helper()
	isolateConfigEnv(t)
	home = t.TempDir()
	state = filepath.Join(home, "state")
	if _, err := install.Manage(configDeclare, install.Request{Action: "install", Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	return home, state
}

func TestRemovalRefusesSettingsTheRecordCannotDescribe(t *testing.T) {
	tests := []struct {
		name string
		file string
		body string
		want string // exact message after "<path>: "
	}{
		{"the file became null", ".cursor/mcp.json", `null`, "settings must be a JSON object"},
		{"the file became an array", ".cursor/hooks.json", `[]`, "settings must be a JSON object"},
		{"the file became text", ".claude.json", `not json`, "settings must be a JSON object"},
		{"the file was emptied", ".claude/settings.json", ``, "settings must be a JSON object"},
		{"mcpServers became an array", ".cursor/mcp.json", `{"mcpServers":[1]}`, "mcpServers is not an object"},
		{"mcpServers became a string", ".claude.json", `{"mcpServers":"x"}`, "mcpServers is not an object"},
		{"hooks became a string", ".cursor/hooks.json", `{"hooks":"x"}`, "hooks is not an object"},
		{"hooks became an array", ".codex/hooks.json", `{"hooks":[]}`, "hooks is not an object"},
		{"a hook event became a string", ".cursor/hooks.json", `{"hooks":{"sessionStart":"x"}}`, "hook event is not an array"},
		{"a hook event became an object", ".claude/settings.json", `{"hooks":{"Stop":{}}}`, "hook event is not an array"},
	}
	for _, action := range []string{"uninstall", "upgrade"} {
		for _, test := range tests {
			t.Run(action+": "+test.name, func(t *testing.T) {
				home, state := installedHome(t)
				path := writeSettings(t, home, test.file, test.body)
				before := installationSnapshot(t, home)

				_, err := install.Manage(configDeclare, install.Request{Action: action, Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: true})
				if err == nil || err.Error() != path+": "+test.want {
					t.Fatalf("error = %v, want %q", err, path+": "+test.want)
				}
				if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
					t.Fatal("a refused removal changed files")
				}
			})
		}
	}
}

// Retained files/sections and exact reformatting are characterized, not
// endorsed; changing them is a deliberate behavior change. These bytes guard
// the complexity refactor, rather than establish a future cleanup policy.
func TestRemovalToleratesSectionsThatAreGone(t *testing.T) {
	tests := []struct {
		name string
		file string
		body string
		want string // exact file content after uninstall
	}{
		{"an empty object", ".cursor/mcp.json", `{}`, "{}\n"},
		{"no mcpServers key", ".cursor/mcp.json", `{"theme":"dark"}`, "{\n  \"theme\": \"dark\"\n}\n"},
		{"a null mcpServers", ".claude.json", `{"mcpServers":null,"a":1}`, "{\n  \"a\": 1,\n  \"mcpServers\": null\n}\n"},
		{"other servers only", ".cursor/mcp.json", `{"mcpServers":{"other":{"command":"x"}}}`, "{\n  \"mcpServers\": {\n    \"other\": {\n      \"command\": \"x\"\n    }\n  }\n}\n"},
		{"no hooks key", ".cursor/hooks.json", `{"version":1}`, "{\n  \"version\": 1\n}\n"},
		{"a null hooks", ".codex/hooks.json", `{"hooks":null}`, "{\n  \"hooks\": null\n}\n"},
		{"hooks without the managed events", ".cursor/hooks.json", `{"version":1,"hooks":{"other":[{"command":"mine"}]}}`,
			"{\n  \"hooks\": {\n    \"other\": [\n      {\n        \"command\": \"mine\"\n      }\n    ]\n  },\n  \"version\": 1\n}\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, state := installedHome(t)
			path := writeSettings(t, home, test.file, test.body)
			if _, err := install.Manage(configDeclare, install.Request{Action: "uninstall", StateDir: state, HomeDir: home, Apply: true}); err != nil {
				t.Fatal(err)
			}
			if got := readFileString(t, path); got != test.want {
				t.Fatalf("file after uninstall = %q, want %q", got, test.want)
			}
			if _, err := os.Stat(filepath.Join(state, "installation.json")); !os.IsNotExist(err) {
				t.Fatalf("the record survived uninstall: %v", err)
			}
		})
	}
}

// Uninstall removes zalki's entries from a hook event but keeps the event as
// an empty list, and keeps entries the user added next to zalki's.
// The empty lists are characterized, not endorsed; changing their retention
// is a deliberate behavior change. Preserving the user's hooks remains required.
func TestUninstallKeepsEmptiedHookEventsAndUserHooks(t *testing.T) {
	home, state := installedHome(t)
	path := filepath.Join(home, ".cursor", "hooks.json")
	var settings map[string]any
	if err := json.Unmarshal([]byte(readFileString(t, path)), &settings); err != nil {
		t.Fatal(err)
	}
	hooks := settings["hooks"].(map[string]any)
	hooks["stop"] = append(hooks["stop"].([]any), map[string]any{"command": "mine"})
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Manage(configDeclare, install.Request{Action: "uninstall", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	var after struct {
		Version int              `json:"version"`
		Hooks   map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFileString(t, path)), &after); err != nil {
		t.Fatal(err)
	}
	if after.Version != 1 {
		t.Errorf("version = %d, want it kept: it is shared with the user's hooks", after.Version)
	}
	want := map[string][]any{
		"beforeSubmitPrompt": {}, "postToolUse": {}, "sessionEnd": {}, "sessionStart": {},
		"stop": {map[string]any{"command": "mine"}},
	}
	if !reflect.DeepEqual(after.Hooks, want) {
		t.Errorf("hooks after uninstall = %v, want %v", after.Hooks, want)
	}
}

// The absence of a settings-path prefix is characterized, not endorsed;
// changing it is a deliberate behavior change.
func TestTamperedRecordPatchesAreRefusedWithExactMessages(t *testing.T) {
	tests := []struct {
		name  string
		patch string
		want  string
	}{
		{"an empty patch", `{}`, "empty installation patch"},
		{"an unknown key", `{"weird":1}`, "unexpected recorded settings key"},
		{"a hook version other than 1", `{"version":2}`, "unsupported recorded hook version"},
		{"mcpServers is not an object", `{"mcpServers":[]}`, "invalid recorded MCP entry"},
		{"mcpServers without zalki", `{"mcpServers":{"other":{"command":"x"}}}`, "invalid recorded MCP entry; expected exactly one zalki server"},
		{"mcpServers with two servers", `{"mcpServers":{"zalki":{"command":"x"},"other":{"command":"y"}}}`, "invalid recorded MCP entry; expected exactly one zalki server"},
		{"zalki is not an object", `{"mcpServers":{"zalki":"x"}}`, "invalid recorded MCP entry; expected exactly one zalki server"},
		{"hooks is not an object", `{"hooks":1}`, "invalid recorded hooks"},
		{"a hook event is not a list", `{"hooks":{"stop":"x"}}`, "invalid recorded hook entries"},
		{"a hook event is an empty list", `{"hooks":{"stop":[]}}`, "invalid recorded hook entries"},
	}
	for _, action := range []string{"uninstall", "upgrade"} {
		for _, test := range tests {
			t.Run(action+": "+test.name, func(t *testing.T) {
				home, state := installedHome(t)
				recordPath := filepath.Join(state, "installation.json")
				var record map[string]any
				if err := json.Unmarshal([]byte(readFileString(t, recordPath)), &record); err != nil {
					t.Fatal(err)
				}
				var patch any
				if err := json.Unmarshal([]byte(test.patch), &patch); err != nil {
					t.Fatal(err)
				}
				record["patches"].(map[string]any)[".cursor/mcp.json"] = patch
				data, _ := json.MarshalIndent(record, "", "  ")
				if err := os.WriteFile(recordPath, append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
				before := installationSnapshot(t, home)

				_, err := install.Manage(configDeclare, install.Request{Action: action, Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: true})
				want := recordActionFailure(action, recordPath, test.want)
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v, want %q", err, want)
				}
				if strings.Contains(err.Error(), filepath.Join(home, ".cursor", "mcp.json")) {
					t.Errorf("a record error should not blame the client settings file: %q", err)
				}
				if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
					t.Fatal("a refused request changed files")
				}
			})
		}
	}
}
