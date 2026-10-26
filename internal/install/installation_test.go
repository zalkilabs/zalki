package install_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/install"
)

func installationSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestInstallationLifecyclePreservesLaterUserChangesAndHistory(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	oldBinary, newBinary := "/old tools/zalki", "/new tools/zalki"
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(home, ".codex", "config.toml")
	originalTOML := "# Keep my zalki notes and comments\nmodel = 'fixture'\n"
	if err := os.WriteFile(tomlPath, []byte(originalTOML), 0600); err != nil {
		t.Fatal(err)
	}
	before := installationSnapshot(t, home)
	preview, err := install.Manage(declare, install.Request{Action: "install", Binary: oldBinary, StateDir: state, HomeDir: home, Apply: false})
	if err != nil || len(preview.Paths) != 6 || !preview.DryRun {
		t.Fatal("bad install preview", preview, err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("preview wrote files")
	}
	if _, err := install.Apply(declare, oldBinary, state, home); err != nil {
		t.Fatal(err)
	}
	before = installationSnapshot(t, home)
	if paths, err := install.Apply(declare, oldBinary, state, home); err != nil || len(paths) != 0 {
		t.Fatal("unchanged install was not idempotent", paths, err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("unchanged install created backups or changed files")
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	var config map[string]any
	if err := readJSON(settings, &config); err != nil {
		t.Fatal(err)
	}
	config["theme"] = "later-user-choice"
	hooks := config["hooks"].(map[string]any)
	hooks["Stop"] = append(hooks["Stop"].([]any), map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "my-later-hook"}}})
	if err := writeJSON(settings, config); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(state, "keep-message.json")
	if err := os.WriteFile(journal, []byte("private historical fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before = installationSnapshot(t, home)
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: newBinary, StateDir: state, HomeDir: home, Apply: false}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("upgrade preview wrote files")
	}
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: newBinary, StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(settings, &config); err != nil {
		t.Fatal(err)
	}
	if config["theme"] != "later-user-choice" {
		t.Fatal("upgrade lost later user changes")
	}
	encoded, _ := json.Marshal(config)
	if bytes.Contains(encoded, []byte(oldBinary)) || !bytes.Contains(encoded, []byte(newBinary)) || !bytes.Contains(encoded, []byte("my-later-hook")) {
		t.Fatal("upgrade replaced unrelated hooks or left old hook paths")
	}
	before = installationSnapshot(t, home)
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: newBinary, StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("unchanged upgrade was not idempotent")
	}
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: false}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("uninstall preview wrote files")
	}
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(settings, &config); err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(config)
	if config["theme"] != "later-user-choice" || !bytes.Contains(encoded, []byte("my-later-hook")) || bytes.Contains(encoded, []byte(newBinary)) {
		t.Fatal("uninstall lost later user edits or left managed hooks")
	}
	if data, err := os.ReadFile(tomlPath); err != nil || string(data) != originalTOML {
		t.Fatal("TOML outside the managed fragment changed", err)
	}
	if data, err := os.ReadFile(journal); err != nil || string(data) != "private historical fixture\n" {
		t.Fatal("uninstall changed history", err)
	}
	if _, err := os.Stat(filepath.Join(state, "installation.json")); !os.IsNotExist(err) {
		t.Fatal("uninstall retained active ownership record", err)
	}
	before = installationSnapshot(t, home)
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err != nil || !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("repeated uninstall was not harmless", err)
	}
}

func TestInstallationConflictsRefusedBeforeAnyChanges(t *testing.T) {
	clearConfigOverrides(t)
	for _, conflict := range []string{"mcp", "toml", "manifest", "malformed-patch"} {
		t.Run(conflict, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, "state")
			if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
				t.Fatal(err)
			}
			switch conflict {
			case "mcp":
				path := filepath.Join(home, ".cursor", "mcp.json")
				data, _ := os.ReadFile(path)
				data = bytes.ReplaceAll(data, []byte("/old/zalki"), []byte("/user-edited/zalki"))
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "toml":
				path := filepath.Join(home, ".codex", "config.toml")
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("extra_setting = true\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "manifest", "malformed-patch":
				path := filepath.Join(state, "installation.json")
				var record install.Installation
				if err := readJSON(path, &record); err != nil {
					t.Fatal(err)
				}
				if conflict == "manifest" {
					record.Home = "/another-home"
				} else {
					record.Patches[".cursor/hooks.json"] = map[string]any{"hooks": "broken"}
				}
				if err := writeJSON(path, record); err != nil {
					t.Fatal(err)
				}
			}
			before := installationSnapshot(t, home)
			for _, action := range []string{"upgrade", "uninstall"} {
				if _, err := install.Manage(declare, install.Request{Action: action, Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err == nil {
					t.Fatal("accepted edited managed configuration", action)
				}
				if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
					t.Fatal("conflict partially changed settings or backups", action)
				}
			}
		})
	}
}

func TestUninstallWithoutRecordDoesNotGuessOwnership(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"zalki":{"command":"other"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := installationSnapshot(t, home)
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("uninstall guessed ownership")
	}
}

func TestCodexTOMLConflictsAndLiteralLookalikes(t *testing.T) {
	clearConfigOverrides(t)
	_, fragment := fixtureConfiguration(t, "/bin/zalki", "/state")
	// A managed-looking table inside a multiline string is not a real server.
	lookalike := []byte("notes = '''" + fragment + "'''\n")
	after, err := install.AddMCPFragment(lookalike, fragment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.RemoveMCPFragment(after, fragment); err == nil {
		t.Fatal("ambiguous literal lookalike was removed")
	}
	// A quoted table is parsed correctly; a conflicting command is refused.
	if _, err := install.AddMCPFragment([]byte("[mcp_servers.\"zalki\"]\ncommand = 'other'\n"), fragment); err == nil {
		t.Fatal("alternate TOML representation overwrote another server")
	}
	if !strings.Contains(fragment, "mcp_servers.zalki") {
		t.Fatal("fixture is not an zalki table")
	}
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(data, '\n'))
}

func TestUnsupportedInstallationSchemaDoesNotChangeConfiguration(t *testing.T) {
	for _, schema := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(schema), func(t *testing.T) {
			clearConfigOverrides(t)
			home := t.TempDir()
			state := filepath.Join(home, "state")
			if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state, "installation.json")
			var record install.Installation
			if err := readJSON(path, &record); err != nil {
				t.Fatal(err)
			}
			record.Schema = schema
			if err := writeJSON(path, record); err != nil {
				t.Fatal(err)
			}
			before := installationSnapshot(t, home)
			for _, apply := range []bool{false, true} {
				if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: apply}); err == nil {
					t.Fatal("accepted unsupported installation schema")
				}
				if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
					t.Fatal("unsupported installation changed configuration")
				}
			}
		})
	}
}

// declare uses the built-in adapters so these tests cover real client configuration.
func declare(binary, state string) ([]install.ConfigFile, error) {
	return adapters.Builtin().Configuration(binary, state)
}
