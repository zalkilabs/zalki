package install_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

func clearConfigOverrides(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
}

func TestInstallationCustomTargetsAndRecordedUninstall(t *testing.T) {
	clearConfigOverrides(t)
	home, custom := t.TempDir(), t.TempDir()
	state := filepath.Join(home, "state")
	codex, claude := filepath.Join(custom, "codex profile"), filepath.Join(custom, "claude profile")
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	// Cursor's CLI config override does not relocate native hooks or MCP files.
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(custom, "cursor"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(custom, "xdg"))
	settings := filepath.Join(claude, "settings.json")
	if err := os.MkdirAll(claude, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"theme":"user-choice"}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := installationSnapshot(t, custom)
	preview, err := install.Manage(declare, install.Request{Action: "install", Binary: "/fixture/zalki", StateDir: state, HomeDir: home, Apply: false})
	if err != nil || len(preview.Paths) != 6 || !reflect.DeepEqual(before, installationSnapshot(t, custom)) {
		t.Fatal("invalid custom preview", preview, err)
	}
	if _, err := install.Apply(declare, "/fixture/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	wanted := map[string]string{
		".codex/hooks.json": filepath.Join(codex, "hooks.json"), ".codex/config.toml": filepath.Join(codex, "config.toml"),
		".claude/settings.json": settings, ".claude.json": filepath.Join(claude, ".claude.json"),
		".cursor/hooks.json": filepath.Join(home, ".cursor/hooks.json"), ".cursor/mcp.json": filepath.Join(home, ".cursor/mcp.json"),
	}
	var record install.Installation
	if err := readJSON(filepath.Join(state, "installation.json"), &record); err != nil || record.Schema != 3 || !reflect.DeepEqual(record.Targets, wanted) {
		t.Fatal("custom destinations not recorded", record, err)
	}
	for key, target := range wanted {
		if _, err := os.Stat(target); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(key, ".cursor/") {
			continue
		}
		if _, err := os.Stat(filepath.Join(home, key)); !os.IsNotExist(err) {
			t.Fatal("wrote default path instead of override", key, err)
		}
	}
	if paths, err := install.Apply(declare, "/fixture/zalki", state, home); err != nil || len(paths) != 0 {
		t.Fatal("custom install was not idempotent", paths, err)
	}
	// Even invalid current environment must not stop removal from recorded paths.
	t.Setenv("CODEX_HOME", "relative-is-invalid")
	t.Setenv("CLAUDE_CONFIG_DIR", "different-relative-profile")
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	for _, target := range wanted {
		data, err := os.ReadFile(target)
		if err != nil || bytes.Contains(data, []byte("/fixture/zalki")) {
			t.Fatal("left managed entry", target, err)
		}
	}
	data, _ := os.ReadFile(settings)
	if !bytes.Contains(data, []byte("user-choice")) {
		t.Fatal("lost unrelated settings")
	}
}

func TestUpgradeKeepsRecordedTargetsAndUninstallInstallMoves(t *testing.T) {
	clearConfigOverrides(t)
	home, custom := t.TempDir(), t.TempDir()
	state := filepath.Join(home, "state")
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(custom, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(custom, "claude"))
	if _, err := install.Manage(declare, install.Request{Action: "install", Binary: "/old/zalki", StateDir: state, HomeDir: home, Apply: true}); err == nil {
		t.Fatal("install silently moved an existing integration")
	}
	preview, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: false})
	wantNotes := []string{
		"Kept " + filepath.Join(home, ".claude.json") + " (CLAUDE_CONFIG_DIR points elsewhere; to move, run zalki uninstall, then zalki install)",
		"Kept " + filepath.Join(home, ".claude/settings.json") + " (CLAUDE_CONFIG_DIR points elsewhere; to move, run zalki uninstall, then zalki install)",
		"Kept " + filepath.Join(home, ".codex/config.toml") + " (CODEX_HOME points elsewhere; to move, run zalki uninstall, then zalki install)",
		"Kept " + filepath.Join(home, ".codex/hooks.json") + " (CODEX_HOME points elsewhere; to move, run zalki uninstall, then zalki install)",
	}
	if err != nil || !reflect.DeepEqual(preview.Notes, wantNotes) {
		t.Fatalf("upgrade notes: %q %v", preview.Notes, err)
	}
	report, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true})
	if err != nil || !reflect.DeepEqual(report.Notes, wantNotes) {
		t.Fatal("upgrade failed", report, err)
	}
	if len(installationSnapshot(t, custom)) != 0 {
		t.Fatal("upgrade wrote to the profile the environment points to")
	}
	for _, rel := range []string{".claude.json", ".claude/settings.json", ".codex/config.toml", ".codex/hooks.json"} {
		data, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil || !bytes.Contains(data, []byte("/new/zalki")) || bytes.Contains(data, []byte("/old/zalki")) {
			t.Fatal("recorded target not upgraded in place", rel, err)
		}
	}
	before := installationSnapshot(t, home)
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil || !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("upgrade not idempotent", err)
	}
	moveByUninstallAndInstall(t, home, state, custom)
}

// moveByUninstallAndInstall is the documented move: uninstall, then install
// in the new environment.
func moveByUninstallAndInstall(t *testing.T, home, state, custom string) {
	t.Helper()
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Apply(declare, "/new/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(custom, "claude", ".claude.json"))
	if err != nil || !bytes.Contains(data, []byte("/new/zalki")) {
		t.Fatal("install did not use the new profile", err)
	}
	if data, _ := os.ReadFile(filepath.Join(home, ".claude.json")); bytes.Contains(data, []byte("zalki")) {
		t.Fatal("uninstall left the old profile entry")
	}
}

func TestInvalidConfigurationTargetsRefuseBeforeWriting(t *testing.T) {
	for _, value := range []string{"relative", "~/profile", "$HOME/profile"} {
		t.Run(value, func(t *testing.T) {
			clearConfigOverrides(t)
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", value)
			if _, err := install.Apply(declare, "/fixture/zalki", filepath.Join(home, "state"), home); err == nil {
				t.Fatal("accepted relative config directory")
			}
			if len(installationSnapshot(t, home)) != 0 {
				t.Fatal("invalid target produced writes")
			}
		})
	}
	t.Run("overlap", func(t *testing.T) {
		clearConfigOverrides(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", filepath.Join(home, ".cursor"))
		if _, err := install.Apply(declare, "/fixture/zalki", filepath.Join(home, "state"), home); err == nil || len(installationSnapshot(t, home)) != 0 {
			t.Fatal("overlapping hook files were accepted", err)
		}
	})
}

func TestSetupRecordsCustomDestinations(t *testing.T) {
	clearConfigOverrides(t)
	home, custom := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	out := filepath.Join(home, "drafts")
	if err := install.WriteSetup(declare, "/fixture/zalki", filepath.Join(home, "state"), out, home, ""); err != nil {
		t.Fatal(err)
	}
	var targets map[string]string
	if err := readJSON(filepath.Join(out, "targets.json"), &targets); err != nil || targets["claude-mcp.json"] != filepath.Join(custom, ".claude.json") {
		t.Fatal("drafts lost their destination", targets, err)
	}
	if len(installationSnapshot(t, custom)) != 0 {
		t.Fatal("drafting modified native configuration")
	}
}

func TestUpgradeLeavesAnotherProfileUntouched(t *testing.T) {
	clearConfigOverrides(t)
	home, custom := t.TempDir(), t.TempDir()
	state := filepath.Join(home, "state")
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	if err := os.WriteFile(filepath.Join(custom, ".claude.json"), []byte(`{"mcpServers":{"zalki":{"command":"user-managed"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	beforeCustom := installationSnapshot(t, custom)
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeCustom, installationSnapshot(t, custom)) {
		t.Fatal("upgrade changed the user's other profile")
	}
	if _, err := install.Apply(declare, "/new/zalki", state, home); err == nil {
		t.Fatal("install moved the integration over a conflicting profile entry")
	}
	if !reflect.DeepEqual(beforeCustom, installationSnapshot(t, custom)) {
		t.Fatal("refused install changed the other profile")
	}
}

func TestRecordedTargetsCannotClaimOtherFiles(t *testing.T) {
	for _, change := range []func(*install.Installation, string){
		func(v *install.Installation, home string) {
			v.Targets[".claude.json"] = filepath.Join(home, "unrelated.json")
		},
		func(v *install.Installation, home string) {
			v.Targets[".cursor/mcp.json"] = filepath.Join(home, "elsewhere/mcp.json")
		},
		func(v *install.Installation, _ string) { delete(v.Targets, ".codex/hooks.json") },
	} {
		clearConfigOverrides(t)
		home := t.TempDir()
		state := filepath.Join(home, "state")
		if _, err := install.Apply(declare, "/fixture/zalki", state, home); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(state, "installation.json")
		var record install.Installation
		if err := readJSON(path, &record); err != nil {
			t.Fatal(err)
		}
		change(&record, home)
		if err := writeJSON(path, record); err != nil {
			t.Fatal(err)
		}
		before := installationSnapshot(t, home)
		if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err == nil || !reflect.DeepEqual(before, installationSnapshot(t, home)) {
			t.Fatal("invalid recorded target produced changes", err)
		}
	}
}
