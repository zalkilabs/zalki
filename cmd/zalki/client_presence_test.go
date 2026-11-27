package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the commands in a home that holds only the clients they name.
// No command is found on the path, so a client is present only when its
// configuration directory is.

func presenceHome(t *testing.T, dirs ...string) string {
	t.Helper()
	for _, name := range []string{"ZALKI_CONFIG", "ZALKI_STATE", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "ZALKI_LOG_LEVEL", "ZALKI_LOG_FILE", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(name, "")
	}
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	for _, dir := range dirs {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

const notFoundCodexCursor = "Not found: Codex, Cursor. Install one, then run zalki install.\n"

func TestInstallConfiguresOnlyTheClientsThatAreThere(t *testing.T) {
	home := presenceHome(t, ".claude")
	state := filepath.Join(home, "state")
	out, diagnostic, err := executeBare(t, home, "", "install", "--state", state, "--apply")
	want := defaultConfigLine(home) +
		"Updated: " + filepath.Join(home, ".claude.json") + "\n" +
		"Updated: " + filepath.Join(home, ".claude", "settings.json") + "\n" +
		notFoundCodexCursor +
		"Existing settings were preserved and backed up. Review/trust integrations in each configured client, then launch normally. Check doctor for adapter and listener requirements.\n"
	if err != nil || diagnostic != "" || out != want {
		t.Fatalf("out %q\nwant %q\ndiagnostic %q, error %v", out, want, diagnostic, err)
	}
	for _, name := range []string{".codex", ".cursor"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was created for a client that is not there (%v)", name, err)
		}
	}
	// No line about the trust of Codex hooks: Codex is not configured.
	if strings.Contains(out, "/hooks") {
		t.Fatalf("output has a notice of a client that was not configured: %q", out)
	}
}

func TestInstallSaysPlainlyWhenNoClientIsThere(t *testing.T) {
	home := presenceHome(t)
	out, _, err := executeBare(t, home, "", "install", "--state", filepath.Join(home, "state"), "--apply")
	const want = "No supported client found: Claude, Codex, Cursor. Install one, then run zalki install."
	if err == nil || err.Error() != want || out != "" {
		t.Fatalf("error %v, output %q; want %q and no output", err, out, want)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatalf("a refused install wrote %v", entries)
	}
}

// One broken file skips its client with the file and the reason, the others are configured, and the exit is not zero.
func TestInstallSkipsAClientWhoseFileCannotBeUsed(t *testing.T) {
	home := presenceHome(t, ".claude", ".codex", ".cursor")
	broken := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(broken, []byte(`{"mcpServers": {"a": 1 "b": 2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := executeBare(t, home, "", "install", "--state", filepath.Join(home, "state"), "--apply")
	if err == nil || !strings.HasPrefix(err.Error(), "skipped Claude:") {
		t.Fatalf("error %v; want a non-zero exit that names the skipped client", err)
	}
	for _, want := range []string{
		"Skipped Claude: " + broken + ": invalid character '\"' after object key:value pair.\n",
		"Next: fix that file, then run zalki install.\n",
		"Updated: " + filepath.Join(home, ".codex", "hooks.json") + "\n",
		"Updated: " + filepath.Join(home, ".cursor", "mcp.json") + "\n",
		"Codex: open /hooks and trust the zalki hooks",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if raw, _ := os.ReadFile(broken); string(raw) != `{"mcpServers": {"a": 1 "b": 2}}` {
		t.Fatalf("the unusable file was changed: %q", raw)
	}
}

// The preview and the upgrade tell the same story; the preview exits non-zero only for an unusable file.
func TestUpgradePreviewListsTheClientsThatAreNotThere(t *testing.T) {
	home := presenceHome(t, ".claude")
	out, _, err := executeBare(t, home, "", "upgrade", "--dry-run", "--config-only", "--state", filepath.Join(home, "state"))
	var report struct {
		Notes []string `json:"notes"`
		Paths []string `json:"paths"`
	}
	if err != nil || json.Unmarshal([]byte(out), &report) != nil {
		t.Fatalf("output %q, error %v", out, err)
	}
	if len(report.Notes) != 1 || report.Notes[0]+"\n" != notFoundCodexCursor || len(report.Paths) != 2 {
		t.Fatalf("notes %q, paths %q", report.Notes, report.Paths)
	}
}

func TestUninstallRemovesWhatTheRecordNamesWhateverIsThere(t *testing.T) {
	home := presenceHome(t, ".claude", ".cursor")
	state := filepath.Join(home, "state")
	if _, _, err := executeBare(t, home, "", "install", "--state", state, "--apply"); err != nil {
		t.Fatal(err)
	}
	out, _, err := executeBare(t, home, "", "uninstall", "--state", state, "--apply")
	var report struct {
		Paths []string `json:"paths"`
	}
	if err != nil || json.Unmarshal([]byte(out), &report) != nil || len(report.Paths) != 4 {
		t.Fatalf("output %q, error %v; want the four files the record names", out, err)
	}
	if _, err := os.Stat(filepath.Join(state, "installation.json")); !os.IsNotExist(err) {
		t.Fatalf("the record is still there (%v)", err)
	}
}

// Every command that skips a client because its file cannot be used says so and exits non-zero.
func TestEveryCommandExitsNonZeroForAnUnusableFile(t *testing.T) {
	for name, args := range map[string][]string{
		"upgrade":         {"upgrade", "--config-only"},
		"upgrade preview": {"upgrade", "--dry-run", "--config-only"},
		"uninstall":       {"uninstall", "--apply"},
		"uninstall plan":  {"uninstall"},
	} {
		t.Run(name, func(t *testing.T) {
			home := presenceHome(t, ".claude", ".cursor")
			state := filepath.Join(home, "state")
			if _, _, err := executeBare(t, home, "", "install", "--state", state, "--apply"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".cursor", "mcp.json"), []byte(`{"mcpServers": `), 0o600); err != nil {
				t.Fatal(err)
			}
			_, diagnostic, err := executeBare(t, home, "", append(append([]string{}, args...), "--state", state)...)
			if err == nil || !strings.HasPrefix(err.Error(), "skipped Cursor:") {
				t.Fatalf("error %v (stderr %q); want a non-zero exit that names the skipped client", err, diagnostic)
			}
		})
	}
}

// The hint tells the user to run the same command again, and that works: after the file is fixed, install configures the client that was skipped.
func TestTheHintAfterASkippedInstallWorks(t *testing.T) {
	home := presenceHome(t, ".claude", ".cursor")
	state := filepath.Join(home, "state")
	broken := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(broken, []byte(`{"hooks": `), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := executeBare(t, home, "", "install", "--state", state, "--apply")
	if err == nil || !strings.Contains(out, "Next: fix that file, then run zalki install.\n") {
		t.Fatalf("error %v, output %q", err, out)
	}
	if err := os.WriteFile(broken, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeBare(t, home, "", "install", "--state", state, "--apply"); err != nil {
		t.Fatalf("the command named by the hint failed: %v", err)
	}
	if raw, _ := os.ReadFile(broken); !strings.Contains(string(raw), "hook") {
		t.Fatalf("install did not configure the client that was skipped: %q", raw)
	}
}
