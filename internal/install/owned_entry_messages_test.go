package install_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests pin the exact sentences the installer shows for the zalki MCP
// entry of a JSON file and for the zalki table of a TOML file: what is wrong and
// what the user can do. The helpers that make them are shared with other kinds of
// entry, so a change of wording or of a label must be a decision.

func jsonEntryPath(home string) string { return filepath.Join(home, ".claude.json") }

func tomlTablePath(home string) string { return filepath.Join(home, ".codex", "config.toml") }

// editJSONFile changes a JSON settings file the user owns.
func editJSONFile(t *testing.T, path string, change func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // the user's numbers keep their digits when the test rewrites the file
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	change(value)
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func zalkiMCPOf(value map[string]any) map[string]any {
	return value["mcpServers"].(map[string]any)["zalki"].(map[string]any)
}

func manageError(t *testing.T, declare install.Declare, home, action, binary string) string {
	t.Helper()
	_, err := install.Manage(declare, goldenRequest(home, action, binary))
	if err == nil {
		t.Fatalf("%s was accepted, want a refusal", action)
	}
	return err.Error()
}

func requireMessage(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("message differs\ngot:  %q\nwant: %q", got, want)
	}
}

func TestJSONEntryRefusalsSayWhatIsWrongAndWhatToDo(t *testing.T) {
	isolateConfigEnv(t)
	t.Run("install over an entry zalki did not write", func(t *testing.T) {
		home := t.TempDir()
		seedHome(t, home, map[string]string{".claude.json": `{"mcpServers":{"zalki":{"command":"mine"}}}`})
		requireMessage(t, manageError(t, configDeclare, home, "install", goldenBinary), jsonEntryPath(home)+`: an existing zalki entry differs in key "args"; run zalki upgrade, or delete the entry and run zalki install again`)
	})
	t.Run("install over an entry that is not an object", func(t *testing.T) {
		home := t.TempDir()
		seedHome(t, home, map[string]string{".claude.json": `{"mcpServers":{"zalki":"x"}}`})
		requireMessage(t, manageError(t, configDeclare, home, "install", goldenBinary), jsonEntryPath(home)+`: an existing zalki MCP server differs; review it before replacing`)
	})
	t.Run("upgrade after the user edited an owned key", func(t *testing.T) {
		home := t.TempDir()
		mustManage(t, home, "install")
		editJSONFile(t, jsonEntryPath(home), func(v map[string]any) { zalkiMCPOf(v)["command"] = "mine" })
		requireMessage(t, manageError(t, configDeclare, home, "upgrade", goldenNewBinary), jsonEntryPath(home)+`: mcpServers.zalki key "command" was edited after Zalki wrote it; restore it or delete that entry, then run zalki upgrade again`)
	})
	t.Run("uninstall after the user edited an owned key", func(t *testing.T) {
		home := t.TempDir()
		mustManage(t, home, "install")
		editJSONFile(t, jsonEntryPath(home), func(v map[string]any) { zalkiMCPOf(v)["command"] = "mine" })
		requireMessage(t, manageError(t, configDeclare, home, "uninstall", goldenBinary), jsonEntryPath(home)+`: mcpServers.zalki key "command" was edited after Zalki wrote it; restore it or delete that entry, then run zalki uninstall again`)
	})
	t.Run("uninstall when the entry is no longer an object", func(t *testing.T) {
		home := t.TempDir()
		mustManage(t, home, "install")
		editJSONFile(t, jsonEntryPath(home), func(v map[string]any) { v["mcpServers"].(map[string]any)["zalki"] = "x" })
		requireMessage(t, manageError(t, configDeclare, home, "uninstall", goldenBinary), jsonEntryPath(home)+`: managed zalki MCP entry was edited; review it before removing or upgrading`)
	})
}

// appDeclare is a client whose MCP entry gains a key in a later release.
func appDeclare(binary, _ string) ([]install.ConfigFile, error) {
	entry := map[string]any{"command": binary}
	if binary == goldenNewBinary {
		entry["type"] = "stdio"
	}
	return []install.ConfigFile{{Path: ".claude.json", Draft: "claude-mcp.json", JSON: map[string]any{"mcpServers": map[string]any{"zalki": entry}}}}, nil
}

func TestUpgradeRefusesAKeyTheUserAlreadySetThatZalkiNowOwns(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	mustManageWith(t, appDeclare, home, "install", goldenBinary)
	editJSONFile(t, jsonEntryPath(home), func(v map[string]any) { zalkiMCPOf(v)["type"] = "sse" })
	requireMessage(t, manageError(t, appDeclare, home, "upgrade", goldenNewBinary), jsonEntryPath(home)+`: mcpServers.zalki key "type" is already set by you to another value; remove it or delete that entry, then run zalki upgrade again`)
}

func mustManageWith(t *testing.T, declare install.Declare, home, action, binary string) {
	t.Helper()
	if _, err := install.Manage(declare, goldenRequest(home, action, binary)); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

// Uninstall removes an entry that holds keys the user added, and says which
// (names only), before and after it happens.
func TestUninstallNamesTheKeysTheUserAddedToTheEntry(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	mustManage(t, home, "install")
	editJSONFile(t, jsonEntryPath(home), func(v map[string]any) { zalkiMCPOf(v)["extra"] = "secret value" })
	path := jsonEntryPath(home)
	notesOf := func(apply bool) []string {
		request := goldenRequest(home, "uninstall", goldenBinary)
		request.Apply = apply
		report, err := install.Manage(configDeclare, request)
		if err != nil {
			t.Fatal(err)
		}
		var notes []string
		for _, note := range report.Notes {
			if strings.HasPrefix(note, path+": ") {
				notes = append(notes, note)
			}
		}
		return notes
	}
	preview := []string{path + `: mcpServers.zalki has keys you added (extra). Uninstall would remove the whole entry with them. Values are not shown. A backup named ` + path + `.zalki-backup-* will keep the old file.`}
	if got := notesOf(false); strings.Join(got, "\n") != strings.Join(preview, "\n") {
		t.Fatalf("preview notes = %q, want %q", got, preview)
	}
	result := []string{path + `: mcpServers.zalki had keys you added (extra). Uninstall removed the whole entry with them. Values are not shown. The backup ` + path + `.zalki-backup-* keeps the old file.`}
	if got := notesOf(true); strings.Join(got, "\n") != strings.Join(result, "\n") {
		t.Fatalf("result notes = %q, want %q", got, result)
	}
}

func TestTOMLTableRefusalsSayWhatIsWrongAndWhatToDo(t *testing.T) {
	isolateConfigEnv(t)
	t.Run("install over a table zalki did not write", func(t *testing.T) {
		home := t.TempDir()
		seedHome(t, home, map[string]string{".codex/config.toml": "[mcp_servers.zalki]\ncommand = \"mine\"\n"})
		requireMessage(t, manageError(t, configDeclare, home, "install", goldenBinary), tomlTablePath(home)+`: an existing zalki table differs in key "args"; run zalki upgrade, or delete the table and run zalki install again`)
	})
	t.Run("uninstall after the user edited an owned key", func(t *testing.T) {
		home := t.TempDir()
		mustManage(t, home, "install")
		raw, err := os.ReadFile(tomlTablePath(home))
		if err != nil {
			t.Fatal(err)
		}
		edited := strings.Replace(string(raw), `command = "/opt/zalki tools/zalki"`, `command = "mine"`, 1)
		if edited == string(raw) {
			t.Fatalf("test setup: the command line was not found in %q", raw)
		}
		if err := os.WriteFile(tomlTablePath(home), []byte(edited), 0o600); err != nil {
			t.Fatal(err)
		}
		requireMessage(t, manageError(t, configDeclare, home, "uninstall", goldenBinary), tomlTablePath(home)+`: [mcp_servers.zalki] key "command" was edited after Zalki wrote it; restore it or delete that table, then run zalki uninstall again`)
	})
}
