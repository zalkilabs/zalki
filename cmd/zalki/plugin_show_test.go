package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

func installReviewInstance(t *testing.T, state, name, version string) string {
	t.Helper()
	manifest := strings.Replace(nativeInstallManifest, `"version":"0.1.0"`, `"version":"`+version+`"`, 1)
	folder := writeInstallFolder(t, manifest, map[string]string{"bin/plugin": "#!/bin/sh\nexit 0\n"})
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	output, err := pluginInstallLocal(t, state, installNoTerminal, "", folder, "--as", name, "--yes", "--allow-unsandboxed", "--tree-sha256", snapshot.TreeSHA256)
	if err != nil {
		t.Fatal(output, err)
	}
	return folder
}

func TestPluginListGroupsInstances(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	installReviewInstance(t, store.Dir, "release", "0.3.0")
	installReviewInstance(t, store.Dir, "build", "0.3.1")
	raw, err := pluginOwnerLocal(t, store.Dir, "list")
	out := string(raw)
	if err != nil || strings.Count(out, "local/demo (author's claim)") != 1 || !strings.Contains(out, "  build  0.3.1  enabled; runtime unknown") || !strings.Contains(out, "  release  0.3.0  enabled; runtime unknown") || strings.Index(out, "  build ") > strings.Index(out, "  release ") {
		t.Fatal("instances were not grouped and sorted by claimed ID/name", out, err)
	}
}

func TestPluginUnprovenInstallIsCurrent(t *testing.T) {
	t.Setenv("ZALKI_ACTOR", "codex:00000000-0000-4000-8000-000000000999")
	t.Setenv("ZALKI_CAPABILITY", "PRIVATE_KEY_SENTINEL")
	store, _ := pluginOwnerFixture(t)
	installReviewInstance(t, store.Dir, "demo", "0.1.0")
	status, err := pluginfeed.NewPluginFeed(store, "demo").PluginStatus()
	if err != nil || status.Install == nil || status.Install.InstalledBy != "unproven" || !status.Enabled {
		t.Fatal("unproven install was not current and enabled", status, err)
	}
	for _, args := range [][]string{{"show", "demo"}, {"list"}} {
		raw, err := pluginOwnerLocal(t, store.Dir, args...)
		if err != nil || !bytes.Contains(raw, []byte("unverified installer")) || bytes.Contains(raw, []byte("old record")) || bytes.Contains(raw, []byte("PRIVATE_KEY_SENTINEL")) {
			t.Fatal("unproven install misrepresented", string(raw), err)
		}
	}
}

func TestPluginShowUnretainedHistoricalFacts(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	folder := installReviewInstance(t, store.Dir, "demo", "0.1.0")
	mustWriteInstallManifest(t, folder, []byte("NEW_PURPOSE_SENTINEL"))
	check := func() {
		for _, args := range [][]string{{"show", "demo"}, {"show", "demo", "--details"}} {
			raw, err := pluginOwnerLocal(t, store.Dir, args...)
			if err != nil || !bytes.Contains(raw, []byte("Purpose: not retained after install.")) || !bytes.Contains(raw, []byte("Staged source copy: not retained after install.")) || bytes.Contains(raw, []byte("NEW_PURPOSE_SENTINEL")) || bytes.Contains(raw, []byte("Read the copied source:")) {
				t.Fatal("show reconstructed historical source or purpose", string(raw), err)
			}
		}
	}
	check()
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	check()
	entries, err := os.ReadDir(filepath.Join(store.Dir, "plugins", ".staging"))
	if err != nil || len(entries) != 0 {
		t.Fatal("successful install retained source staging", entries, err)
	}
}

func TestPluginOwnerWideningRequiresTerminal(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	installReviewInstance(t, store.Dir, "demo", "0.1.0")
	for _, args := range [][]string{{"grant", "demo", "--status", "--receive"}, {"configure", "demo", "--scope", "all"}} {
		cmd := newRootCommand(t.TempDir())
		var output bytes.Buffer
		cmd.SetIn(strings.NewReader("yes\n"))
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs(append([]string{"--state", store.Dir, "--log-level", "off", "plugins"}, args...))
		if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "in your own terminal") {
			t.Fatal("nonterminal widened plugin access", output.String(), err)
		}
	}
}

func TestPluginListRejectsSymlinkRecord(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	installReviewInstance(t, store.Dir, "demo", "0.1.0")
	record := filepath.Join(store.Dir, "plugins", "demo.json")
	outside := filepath.Join(t.TempDir(), "private-record")
	if err := os.Rename(record, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, record); err != nil {
		t.Fatal(err)
	}
	raw, err := pluginOwnerLocal(t, store.Dir, "list")
	if err != nil || !bytes.Contains(raw, []byte("demo  unreadable; run zalki doctor")) || bytes.Contains(raw, []byte("local/demo")) {
		t.Fatal("list followed or hid unsafe record", string(raw), err)
	}
}
