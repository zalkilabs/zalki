package install_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests cover keys the user adds to the managed zalki MCP entry. Upgrade
// keeps them and rewrites only the keys zalki owns; it refuses only when an
// owned key was edited. They use the real Claude, Cursor and Codex declarations.

func ukInstalled(t *testing.T, binary string) (home, state string) {
	t.Helper()
	clearConfigOverrides(t)
	home = t.TempDir()
	state = filepath.Join(home, "state")
	if _, err := install.Apply(declare, binary, state, home); err != nil {
		t.Fatal(err)
	}
	return home, state
}

func ukUpgrade(home, state, binary string) error {
	_, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: binary, StateDir: state, HomeDir: home, Apply: true})
	return err
}

func ukWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func ukDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := readJSON(path, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func ukServers(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	servers, ok := doc["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers is not an object")
	}
	return servers
}

func ukEntry(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	entry, ok := ukServers(t, doc)["zalki"].(map[string]any)
	if !ok {
		t.Fatal("the zalki entry is missing")
	}
	return entry
}

// ukAddJSONKeys adds the user's own keys and another server to a JSON MCP file.
func ukAddJSONKeys(t *testing.T, path string) {
	t.Helper()
	doc := ukDoc(t, path)
	entry := ukEntry(t, doc)
	entry["env"] = map[string]any{"FOO": "1"}
	entry["disabled"] = false
	ukServers(t, doc)["other"] = map[string]any{"command": "keep"}
	if err := writeJSON(path, doc); err != nil {
		t.Fatal(err)
	}
}

func ukCodexPath(home string) string { return filepath.Join(home, ".codex", "config.toml") }

const ukCodexUserKeys = "default_tools_approval_mode = \"approve\"\n\n[mcp_servers.zalki.env]\nFOO = \"1\"\n"

func TestUpgradeKeepsUserKeysInTheCodexTable(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	config := ukCodexPath(home)
	edited := readFileString(t, config) + ukCodexUserKeys
	ukWrite(t, config, edited)
	if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(edited, "\"/old/zalki\"", "\"/new/zalki\"", 1)
	if got := readFileString(t, config); got != want {
		t.Fatalf("upgrade changed more than the owned values\n got %q\nwant %q", got, want)
	}
	var record install.Installation
	if err := readJSON(filepath.Join(state, "installation.json"), &record); err != nil {
		t.Fatal(err)
	}
	if _, fragment := fixtureConfiguration(t, "/new/zalki", state); record.Fragments[".codex/config.toml"] != fragment {
		t.Fatal("the record does not name the new fragment only")
	}
}

func TestUpgradeKeepsUserKeysInClaudeAndCursorEntries(t *testing.T) {
	for _, file := range []string{".claude.json", ".cursor/mcp.json"} {
		t.Run(file, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			path := filepath.Join(home, file)
			ukAddJSONKeys(t, path)
			if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
				t.Fatal(err)
			}
			doc := ukDoc(t, path)
			entry := ukEntry(t, doc)
			args, _ := entry["args"].([]any)
			if entry["command"] != "/new/zalki" || len(args) == 0 || args[len(args)-1] != "mcp" {
				t.Fatal("owned keys were not rewritten", entry)
			}
			if entry["disabled"] != false || !reflect.DeepEqual(entry["env"], map[string]any{"FOO": "1"}) {
				t.Fatal("the user's keys were not kept", entry)
			}
			if !reflect.DeepEqual(ukServers(t, doc)["other"], map[string]any{"command": "keep"}) {
				t.Fatal("another server changed")
			}
		})
	}
}

func TestUpgradeRefusesAnEditedOwnedKeyInEveryClient(t *testing.T) {
	tests := []struct{ file, want string }{
		{".codex/config.toml", "[mcp_servers.zalki] key \"command\" was edited after Zalki wrote it"},
		{".claude.json", "mcpServers.zalki key \"command\" was edited after Zalki wrote it"},
		{".cursor/mcp.json", "mcpServers.zalki key \"command\" was edited after Zalki wrote it"},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			path := filepath.Join(home, test.file)
			ukWrite(t, path, strings.Replace(readFileString(t, path), "/old/zalki", "/hand/zalki", 1))
			before := installationSnapshot(t, home)
			err := ukUpgrade(home, state, "/new/zalki")
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), path) {
				t.Fatal("edited owned key was not refused with the clear message", err)
			}
			if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
				t.Fatal("a refused upgrade changed files")
			}
		})
	}
}

func TestInstallRerunAcceptsUserKeysAndRefusesADifferentOwnedKey(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	config := ukCodexPath(home)
	ukWrite(t, config, readFileString(t, config)+ukCodexUserKeys)
	ukAddJSONKeys(t, filepath.Join(home, ".claude.json"))
	before := installationSnapshot(t, home)
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal("install over an entry with user keys was refused", err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("an install that had nothing to change wrote files")
	}
	ukWrite(t, config, strings.Replace(readFileString(t, config), "/old/zalki", "/hand/zalki", 1))
	before = installationSnapshot(t, home)
	_, err := install.Apply(declare, "/old/zalki", state, home)
	if err == nil || !strings.Contains(err.Error(), "an existing zalki table differs in key \"command\"") {
		t.Fatal("install over a different owned key was not refused", err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("a refused install changed files")
	}
}

func TestUpgradeRefusalInOneFileLeavesEveryFileUnchanged(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	config := ukCodexPath(home)
	ukWrite(t, config, readFileString(t, config)+ukCodexUserKeys) // would upgrade cleanly
	cursor := filepath.Join(home, ".cursor", "mcp.json")
	ukWrite(t, cursor, strings.Replace(readFileString(t, cursor), "/old/zalki", "/hand/zalki", 1))
	before := installationSnapshot(t, home)
	if err := ukUpgrade(home, state, "/new/zalki"); err == nil || !strings.Contains(err.Error(), cursor) {
		t.Fatal("the edited Cursor entry was not refused", err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("a refusal in one file changed another file or the record")
	}
}

func TestUpgradeBacksUpTheFileWithItsUserKeys(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	config := ukCodexPath(home)
	edited := readFileString(t, config) + ukCodexUserKeys
	ukWrite(t, config, edited)
	if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
		t.Fatal(err)
	}
	backups, err := filepath.Glob(config + ".zalki-backup-*")
	if err != nil || len(backups) != 1 || readFileString(t, backups[0]) != edited {
		t.Fatal("the backup does not hold the file as it was before the upgrade", backups, err)
	}
}

func TestUpgradeAcrossReleasesAndBackKeepsUserKeys(t *testing.T) {
	home, state := ukInstalled(t, "/a/zalki")
	config, claude := ukCodexPath(home), filepath.Join(home, ".claude.json")
	ukWrite(t, config, readFileString(t, config)+ukCodexUserKeys)
	ukAddJSONKeys(t, claude)
	wantCodex, wantClaude := readFileString(t, config), ukDoc(t, claude)
	for _, binary := range []string{"/b/zalki", "/a/zalki"} {
		if err := ukUpgrade(home, state, binary); err != nil {
			t.Fatal(err)
		}
	}
	if readFileString(t, config) != wantCodex || !reflect.DeepEqual(ukDoc(t, claude), wantClaude) {
		t.Fatal("A to B to A did not return the user's configuration")
	}
}

func ukHookEntries(t *testing.T, doc map[string]any, event string) []any {
	t.Helper()
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks is not an object")
	}
	entries, ok := hooks[event].([]any)
	if !ok {
		t.Fatal("hook event is not a list", event)
	}
	return entries
}

// A hook entry the user edited stays as theirs and zalki appends its own, so two
// zalki hooks can run. This pins today's behavior; the follow-up is ZAL-104.
func TestUpgradeKeepsAUserEditedHookEntryAndAddsOwn(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	path := filepath.Join(home, ".claude", "settings.json")
	doc := ukDoc(t, path)
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok || len(hooks) == 0 {
		t.Fatal("no hooks recorded")
	}
	event := slices.Sorted(maps.Keys(hooks))[0]
	entries := ukHookEntries(t, doc, event)
	entry, ok := entries[0].(map[string]any)
	if !ok || len(entries) != 1 {
		t.Fatal("unexpected hook entries", entries)
	}
	entry["user_note"] = true
	if err := writeJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
		t.Fatal(err)
	}
	after := ukHookEntries(t, ukDoc(t, path), event)
	joined, err := json.Marshal(after)
	if err != nil || len(after) != 2 || !strings.Contains(string(joined), "user_note") || !strings.Contains(string(joined), "/new/zalki") {
		t.Fatal("expected the edited entry and a new zalki entry", string(joined), err)
	}
}
