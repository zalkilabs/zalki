package install_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

func TestUpgradeFreshStateMatchesInstall(t *testing.T) {
	clearConfigOverrides(t)
	for _, existingDir := range []bool{false, true} {
		home := t.TempDir()
		state := filepath.Join(home, "state")
		if existingDir {
			if err := os.Mkdir(state, 0700); err != nil {
				t.Fatal(err)
			}
		}
		request := install.Request{Action: "install", Binary: "/fixture/zalki", StateDir: state, HomeDir: home}
		want, err := install.Manage(declare, request)
		if err != nil {
			t.Fatal(err)
		}
		before := installationSnapshot(t, home)
		request.Action = "upgrade"
		got, err := install.Manage(declare, request)
		if err != nil || got.Action != "upgrade" || !reflect.DeepEqual(want.Paths, got.Paths) || !got.DryRun {
			t.Fatal("fresh upgrade differs from install", want, got, err)
		}
		if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
			t.Fatal("fresh plan wrote files")
		}
		request.Apply = true
		if _, err := install.Manage(declare, request); err != nil {
			t.Fatal(err)
		}
		checkFreshManaged(t, home, "/fixture/zalki", "")
	}
}

func TestUpgradeFreshPreservesOwnerEntriesAndBackup(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	original := `{"crossSessionInbound":true,"theme":"owner-choice"}`
	writeSettings(t, home, ".claude/settings.json", original)
	request := install.Request{Action: "upgrade", Binary: "/fixture/zalki", StateDir: state, HomeDir: home, Apply: true}
	if _, err := install.Manage(declare, request); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".claude/settings.json")
	raw, _ := os.ReadFile(settings)
	if !bytes.Contains(raw, []byte(`"crossSessionInbound": true`)) || !bytes.Contains(raw, []byte("owner-choice")) {
		t.Fatal("owner entries dropped", string(raw))
	}
	backups, err := filepath.Glob(settings + ".zalki-backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("fresh upgrade lost backup", backups, err)
	}
	before, _ := os.ReadFile(backups[0])
	if string(before) != original {
		t.Fatal("backup was not original bytes", string(before))
	}
}

func TestUpgradePendingOwnershipKeepsTargets(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	request := install.Request{Action: "install", Binary: "/old/zalki", StateDir: state, HomeDir: home, Apply: true}
	if _, err := install.CrashAfter(declare, request, -1); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(other, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(other, "claude"))
	request.Action, request.Binary = "upgrade", "/new/zalki"
	if _, err := install.Manage(declare, request); err != nil {
		t.Fatal(err)
	}
	checkFreshManaged(t, home, "/new/zalki", "/old/zalki")
	entries, _ := os.ReadDir(other)
	if len(entries) != 0 {
		t.Fatal("pending recovery moved profiles", entries)
	}
}

func TestUpgradeFreshNativeConflictRefuses(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	writeSettings(t, home, ".claude.json", `{"mcpServers":{"zalki":{"command":"owner-command"}}}`)
	before := installationSnapshot(t, home)
	_, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/fixture/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true})
	if err == nil || !strings.Contains(err.Error(), "zalki") || !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("fresh upgrade overwrote conflicting entry", err)
	}
}

func checkFreshManaged(t *testing.T, home, want, gone string) {
	t.Helper()
	for _, rel := range managedFiles {
		raw, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil || !bytes.Contains(raw, []byte(want)) {
			t.Fatal("missing managed binary", rel, err)
		}
		if gone != "" && bytes.Contains(raw, []byte(gone)) {
			t.Fatal("old managed binary retained", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "state", "installation.pending.json")); !os.IsNotExist(err) {
		t.Fatal("pending record not cleared", err)
	}
}

func recordFailure(path, reason string) string {
	return fmt.Sprintf("installation record %q is invalid: %s. Restore a valid record before retrying.", path, reason)
}

func recordActionFailure(action, path, reason string) string {
	if action == "upgrade" {
		return recordFailure(path, reason)
	}
	return reason
}
