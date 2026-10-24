package bus

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	preview, err := ManageInstallation(InstallationRequest{Action: "install", Binary: "/fixture/zalki", StateDir: state, HomeDir: home, Apply: false})
	if err != nil || len(preview.Paths) != 6 || !reflect.DeepEqual(before, installationSnapshot(t, custom)) {
		t.Fatal("invalid custom preview", preview, err)
	}
	if _, err := Install("/fixture/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	wanted := map[string]string{
		".codex/hooks.json": filepath.Join(codex, "hooks.json"), ".codex/config.toml": filepath.Join(codex, "config.toml"),
		".claude/settings.json": settings, ".claude.json": filepath.Join(claude, ".claude.json"),
		".cursor/hooks.json": filepath.Join(home, ".cursor/hooks.json"), ".cursor/mcp.json": filepath.Join(home, ".cursor/mcp.json"),
	}
	var record installation
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
	if paths, err := Install("/fixture/zalki", state, home); err != nil || len(paths) != 0 {
		t.Fatal("custom install was not idempotent", paths, err)
	}
	// Even invalid current environment must not stop removal from recorded paths.
	t.Setenv("CODEX_HOME", "relative-is-invalid")
	t.Setenv("CLAUDE_CONFIG_DIR", "different-relative-profile")
	if _, err := ManageInstallation(InstallationRequest{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err != nil {
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

func TestInstallationMovesRecordedTargetsToProfile(t *testing.T) {
	clearConfigOverrides(t)
	home, custom := t.TempDir(), t.TempDir()
	state := filepath.Join(home, "state")
	if _, err := Install("/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(state, "installation.json")
	var record installation
	if err := readJSON(manifest, &record); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(custom, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(custom, "claude"))
	before := installationSnapshot(t, home)
	if _, err := ManageInstallation(InstallationRequest{Action: "install", Binary: "/old/zalki", StateDir: state, HomeDir: home, Apply: true}); err == nil {
		t.Fatal("install silently moved an existing integration")
	}
	if _, err := ManageInstallation(InstallationRequest{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: false}); err != nil || !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("migration preview changed files", err)
	}
	if _, err := ManageInstallation(InstallationRequest{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	for key := range record.Patches {
		if strings.HasPrefix(key, ".cursor/") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(home, key))
		if err != nil || bytes.Contains(data, []byte("/old/zalki")) {
			t.Fatal("old integration not removed", key, err)
		}
	}
	before = installationSnapshot(t, home)
	if _, err := ManageInstallation(InstallationRequest{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil || !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("profile upgrade not idempotent", err)
	}
	// Move again, including old/new paths outside the default home.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(custom, "claude-next"))
	if _, err := ManageInstallation(InstallationRequest{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(custom, "claude", ".claude.json"))
	if bytes.Contains(data, []byte("/new/zalki")) {
		t.Fatal("old profile retained its managed MCP entry")
	}
	if _, err := os.Stat(filepath.Join(custom, "claude-next", ".claude.json")); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidConfigurationTargetsRefuseBeforeWriting(t *testing.T) {
	for _, value := range []string{"relative", "~/profile", "$HOME/profile"} {
		t.Run(value, func(t *testing.T) {
			clearConfigOverrides(t)
			home := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", value)
			if _, err := Install("/fixture/zalki", filepath.Join(home, "state"), home); err == nil {
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
		if _, err := Install("/fixture/zalki", filepath.Join(home, "state"), home); err == nil || len(installationSnapshot(t, home)) != 0 {
			t.Fatal("overlapping hook files were accepted", err)
		}
	})
}

func TestSetupRecordsCustomDestinations(t *testing.T) {
	clearConfigOverrides(t)
	home, custom := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	out := filepath.Join(home, "drafts")
	if err := WriteSetup("/fixture/zalki", filepath.Join(home, "state"), out, home); err != nil {
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

func TestProfileMigrationConflictPreservesBothProfiles(t *testing.T) {
	clearConfigOverrides(t)
	home, custom := t.TempDir(), t.TempDir()
	state := filepath.Join(home, "state")
	if _, err := Install("/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	if err := os.WriteFile(filepath.Join(custom, ".claude.json"), []byte(`{"mcpServers":{"zalki":{"command":"user-managed"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	beforeHome, beforeCustom := installationSnapshot(t, home), installationSnapshot(t, custom)
	if _, err := ManageInstallation(InstallationRequest{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err == nil {
		t.Fatal("overwrote a conflicting profile entry")
	}
	if !reflect.DeepEqual(beforeHome, installationSnapshot(t, home)) || !reflect.DeepEqual(beforeCustom, installationSnapshot(t, custom)) {
		t.Fatal("refused migration partially changed configuration")
	}
}

func TestRecordedTargetsCannotClaimOtherFiles(t *testing.T) {
	for _, change := range []func(*installation, string){
		func(v *installation, home string) { v.Targets[".claude.json"] = filepath.Join(home, "unrelated.json") },
		func(v *installation, home string) {
			v.Targets[".cursor/mcp.json"] = filepath.Join(home, "elsewhere/mcp.json")
		},
		func(v *installation, _ string) { delete(v.Targets, ".codex/hooks.json") },
	} {
		clearConfigOverrides(t)
		home := t.TempDir()
		state := filepath.Join(home, "state")
		if _, err := Install("/fixture/zalki", state, home); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(state, "installation.json")
		var record installation
		if err := readJSON(path, &record); err != nil {
			t.Fatal(err)
		}
		change(&record, home)
		if err := writeJSON(path, record); err != nil {
			t.Fatal(err)
		}
		before := installationSnapshot(t, home)
		if _, err := ManageInstallation(InstallationRequest{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err == nil || !reflect.DeepEqual(before, installationSnapshot(t, home)) {
			t.Fatal("invalid recorded target produced changes", err)
		}
	}
}

func TestCodexQueueUsesRecipientProfile(t *testing.T) {
	bin, recipient := t.TempDir(), t.TempDir()
	output := filepath.Join(bin, "profile")
	script := "#!/bin/sh\nprintf '%s' \"$CODEX_HOME\" > " + shellQuote(output) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", filepath.Join(bin, "sender-profile"))
	receipt := queueCodex(context.Background(), Message{ID: newID(), Body: "profile test"}, Session{Kind: "codex", ID: "codex:" + newID(), NativeConfigDir: recipient})
	data, err := os.ReadFile(output)
	if err != nil || string(data) != recipient || receipt.Status != StatusSubmitted {
		t.Fatal("queue inherited sender profile", string(data), receipt, err)
	}
}

func TestAdapterDefinesItsOwnConfigurationOverride(t *testing.T) {
	home, profile := t.TempDir(), t.TempDir()
	t.Setenv("FOURTH_CONFIG_DIR", profile)
	adapter := configFixtureAdapter{
		fixtureAdapter: fixtureAdapter{kind: "fourth", inbox: InboxNative},
		files:          []ConfigFile{{Path: ".fourth/mcp.json", ConfigRootEnv: "FOURTH_CONFIG_DIR", Draft: "fourth-mcp.json", JSON: mcpConfig("/fixture/zalki", nil, false)}},
	}
	registry, err := NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "state")
	if _, err := registry.manageInstallation(InstallationRequest{Action: "install", Binary: "/fixture/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(profile, "mcp.json")
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOURTH_CONFIG_DIR", "")
	if _, err := registry.manageInstallation(InstallationRequest{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if bytes.Contains(data, []byte("/fixture/zalki")) {
		t.Fatal("fourth adapter could not uninstall from its recorded profile")
	}
}
