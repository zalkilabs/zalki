package install_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

func TestInstallPreservesSettingsAndIsIdempotent(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	original := []byte(`{"theme":"existing","hooks":{"Stop":[{"hooks":[{"type":"command","command":"existing-hook"}]}]}}`)
	if err := os.WriteFile(settings, original, 0600); err != nil {
		t.Fatal(err)
	}
	paths, err := install.Apply(declare, "/opt/local/bin/zalki", filepath.Join(home, "state"), home)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 6 {
		t.Fatalf("expected six configuration additions, got %d", len(paths))
	}
	if _, err := install.Apply(declare, "/opt/local/bin/zalki", filepath.Join(home, "state"), home); err != nil {
		t.Fatal(err)
	}
	var merged map[string]any
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &merged); err != nil {
		t.Fatal(err)
	}
	if merged["theme"] != "existing" {
		t.Fatal("unrelated settings were replaced")
	}
	entries := merged["hooks"].(map[string]any)["Stop"].([]any)
	if len(entries) != 2 {
		t.Fatalf("existing hook lost or duplicated: %d", len(entries))
	}
	backups, err := filepath.Glob(settings + ".zalki-backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("missing backups: %v %v", backups, err)
	}
	foundOriginal := false
	for _, path := range backups {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) == string(original) {
			foundOriginal = true
		}
	}
	if !foundOriginal {
		t.Fatal("original settings were not backed up byte-for-byte")
	}
	cursor := filepath.Join(home, ".cursor", "hooks.json")
	if err := readJSON(cursor, &merged); err != nil {
		t.Fatal(err)
	}
	if merged["version"] != float64(1) {
		t.Fatal("missing Cursor hook schema version")
	}
	stop := merged["hooks"].(map[string]any)["stop"].([]any)[0].(map[string]any)
	if stop["command"] == nil || stop["hooks"] != nil {
		t.Fatal("Cursor was configured with another client's hook schema")
	}
}

func TestInstallRefusesConflictingServerWithoutPartialChanges(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	config := filepath.Join(home, ".claude.json")
	before := []byte(`{"mcpServers":{"zalki":{"command":"someone-elses-server"}},"other":true}`)
	if err := os.WriteFile(config, before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Apply(declare, "/bin/zalki", filepath.Join(home, "state"), home); err == nil {
		t.Fatal("replaced existing MCP server")
	}
	after, err := os.ReadFile(config)
	if err != nil || string(after) != string(before) {
		t.Fatal("conflicting settings were modified")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("partially applied other settings")
	}
}

func TestInstallRefusesAlternateTOMLAndSymlinkedFiles(t *testing.T) {
	clearConfigOverrides(t)
	for _, body := range []string{`mcp_servers = { zalki = { command = "other" } }`, "[mcp_servers.\"zalki\"]\ncommand = \"other\"\n"} {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := install.Apply(declare, "/bin/zalki", filepath.Join(home, "state"), home); err == nil {
			t.Fatal("accepted unmergeable TOML")
		}
	}
	home := t.TempDir()
	other := filepath.Join(home, "other.json")
	if err := os.WriteFile(other, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Apply(declare, "/bin/zalki", filepath.Join(home, "state"), home); err == nil {
		t.Fatal("followed symlinked config file")
	}
}

func TestSetupQuotesPathsAndWritesOnlyDrafts(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	out := filepath.Join(home, "drafts")
	binary := "/tmp/agent's tools/$literal/zalki"
	if err := install.WriteSetup(declare, binary, filepath.Join(home, "state"), out, home); err != nil {
		t.Fatal(err)
	}
	if err := install.WriteSetup(declare, binary, filepath.Join(home, "state"), out, home); err == nil {
		t.Fatal("overwrote setup drafts")
	}
	data, err := os.ReadFile(filepath.Join(out, "cursor-hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	command := value["hooks"].(map[string]any)["sessionStart"].([]any)[0].(map[string]any)["command"].(string)
	if !strings.HasPrefix(command, install.ShellQuote(binary)) {
		t.Fatal("command path is not safely shell quoted")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatal("setup unexpectedly installed configuration")
	}
}
