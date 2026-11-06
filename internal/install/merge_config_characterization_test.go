package install_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests pin how install and upgrade merge zalki's entries into a client
// settings file that already exists: which unusual contents are refused, with
// which exact message, and that a refusal leaves every file untouched. The
// settings files are the user's; they are written by hand in these tests.
// Exact error prefixes and the empty-versus-whitespace distinction below are
// characterized, not endorsed; changing them is a deliberate behavior change.

func writeSettings(t *testing.T, home, rel, body string) string {
	t.Helper()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInstallRefusesSettingsItCannotMerge(t *testing.T) {
	const (
		objectOnly = "settings must be a JSON object"
		serversNot = "existing mcpServers is not an object"
		hooksNot   = "existing hooks is not an object"
		eventNot   = "existing hook event is not an array"
		versionBad = "unsupported existing Cursor hook schema version"
	)
	tests := []struct {
		name string
		file string
		body string
		want string // exact message after "<path>: "
		as   any    // or a decoder error type the message wraps
	}{
		{name: "mcpServers is an array", file: ".cursor/mcp.json", body: `{"mcpServers":[]}`, want: serversNot},
		{name: "mcpServers is a string", file: ".cursor/mcp.json", body: `{"mcpServers":"x"}`, want: serversNot},
		{name: "mcpServers is a number", file: ".claude.json", body: `{"mcpServers":5}`, want: serversNot},
		{name: "hooks is a number", file: ".cursor/hooks.json", body: `{"hooks":5}`, want: hooksNot},
		{name: "hooks is an array", file: ".claude/settings.json", body: `{"hooks":[]}`, want: hooksNot},
		{name: "a hook event is a string", file: ".cursor/hooks.json", body: `{"hooks":{"sessionStart":"x"}}`, want: eventNot},
		{name: "a hook event is an object", file: ".codex/hooks.json", body: `{"hooks":{"Stop":{}}}`, want: eventNot},
		{name: "version is 2", file: ".cursor/hooks.json", body: `{"version":2}`, want: versionBad},
		{name: "version is a string", file: ".cursor/hooks.json", body: `{"version":"1"}`, want: versionBad},
		{name: "version is 0", file: ".cursor/hooks.json", body: `{"version":0}`, want: versionBad},
		{name: "the file is null", file: ".cursor/mcp.json", body: `null`, want: objectOnly},
		{name: "the file is an array", file: ".cursor/mcp.json", body: `[]`, as: &json.UnmarshalTypeError{}},
		{name: "the file is a string", file: ".claude.json", body: `"text"`, as: &json.UnmarshalTypeError{}},
		{name: "the file is a number", file: ".claude/settings.json", body: `5`, as: &json.UnmarshalTypeError{}},
		{name: "the file is cut short", file: ".cursor/mcp.json", body: `{"mcpServers":`, as: &json.SyntaxError{}},
		{name: "the file is only whitespace", file: ".cursor/mcp.json", body: "  \n", as: &json.SyntaxError{}},
		{name: "the file is not JSON", file: ".cursor/hooks.json", body: `hooks: yes`, as: &json.SyntaxError{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateConfigEnv(t)
			home := t.TempDir()
			path := writeSettings(t, home, test.file, test.body)
			before := installationSnapshot(t, home)

			_, err := install.Manage(configDeclare, install.Request{Action: "install", Binary: "/bin/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true})
			if err == nil {
				t.Fatalf("install merged %s into %s", path, test.body)
			}
			if test.want != "" && err.Error() != path+": "+test.want {
				t.Fatalf("error = %q, want %q", err, path+": "+test.want)
			}
			switch kind := test.as.(type) {
			case *json.UnmarshalTypeError:
				if !errors.As(err, &kind) {
					t.Fatalf("error = %v (%T), want a wrapped *json.UnmarshalTypeError", err, err)
				}
			case *json.SyntaxError:
				if !errors.As(err, &kind) {
					t.Fatalf("error = %v (%T), want a wrapped *json.SyntaxError", err, err)
				}
			}
			if test.as != nil && !strings.HasPrefix(err.Error(), path+": ") {
				t.Fatalf("error %q does not name the file first", err)
			}
			if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
				t.Fatal("a refused install changed files")
			}
		})
	}
}

// Empty files and null sections are characterized, not endorsed; changing
// their acceptance or replacement is a deliberate behavior change.
func TestInstallMergesUnusualButAcceptableSettings(t *testing.T) {
	tests := []struct {
		name string
		file string
		body string
		keep map[string]any // top-level keys that must survive the merge
		need string         // top-level key that must exist afterwards
	}{
		{name: "an empty file counts as an empty object", file: ".cursor/mcp.json", body: ``, need: "mcpServers"},
		{name: "a null mcpServers is replaced", file: ".cursor/mcp.json", body: `{"mcpServers":null,"mine":1}`, keep: map[string]any{"mine": float64(1)}, need: "mcpServers"},
		{name: "a null hooks is replaced", file: ".cursor/hooks.json", body: `{"hooks":null,"mine":"x"}`, keep: map[string]any{"mine": "x"}, need: "hooks"},
		{name: "version 1 is accepted", file: ".cursor/hooks.json", body: `{"version":1,"mine":true}`, keep: map[string]any{"version": float64(1), "mine": true}, need: "hooks"},
		{name: "other servers are kept", file: ".cursor/mcp.json", body: `{"mcpServers":{"other":{"command":"x"}}}`, need: "mcpServers"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateConfigEnv(t)
			home := t.TempDir()
			path := writeSettings(t, home, test.file, test.body)
			if _, err := install.Manage(configDeclare, install.Request{Action: "install", Binary: "/bin/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true}); err != nil {
				t.Fatal(err)
			}
			var merged map[string]any
			if err := json.Unmarshal([]byte(readFileString(t, path)), &merged); err != nil {
				t.Fatalf("merged file is not valid JSON: %v", err)
			}
			for key, want := range test.keep {
				if !reflect.DeepEqual(merged[key], want) {
					t.Errorf("%s = %v, want %v", key, merged[key], want)
				}
			}
			if merged[test.need] == nil {
				t.Errorf("%s is missing after the merge: %v", test.need, merged)
			}
			if test.name == "other servers are kept" {
				servers, ok := merged["mcpServers"].(map[string]any)
				if !ok {
					t.Fatalf("mcpServers = %T, want an object", merged["mcpServers"])
				}
				if !reflect.DeepEqual(servers["other"], map[string]any{"command": "x"}) || servers["zalki"] == nil {
					t.Errorf("servers after merge = %v, want the user's server and zalki", servers)
				}
			}
		})
	}
}

func TestInstallRefusesAPatchKeyNoClientDeclares(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	declare := func(string, string) ([]install.ConfigFile, error) {
		return []install.ConfigFile{{Path: ".solo/settings.json", Draft: "solo.json", JSON: map[string]any{"weird": map[string]any{"on": true}}}}, nil
	}
	path := writeSettings(t, home, ".solo/settings.json", `{"mine":1}`)
	before := installationSnapshot(t, home)
	_, err := install.Manage(declare, install.Request{Action: "install", Binary: "/bin/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true})
	if err == nil || err.Error() != path+": unexpected settings key" {
		t.Fatalf("error = %v, want %q", err, path+": unexpected settings key")
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("a refused install changed files")
	}
}
