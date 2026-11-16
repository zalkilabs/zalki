package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginOldRecordRecoveryKeepsDataAfterInstallRefusal(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ZALKI_ACTOR", "")
	t.Setenv("ZALKI_CAPABILITY", "")
	store, _ := pluginOwnerFixture(t)
	dir := filepath.Join(store.Dir, "plugins")
	if err := os.MkdirAll(filepath.Join(dir, "ext.data"), 0700); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{filepath.Join(dir, "ext.json"): []byte(`{"capabilities":{"inbox_read":true}}`)}
	before[filepath.Join(dir, "ext.data", "keep")] = []byte("owned plugin data\n")
	for path, raw := range before {
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := pluginOwnerLocal(t, store.Dir, "list")
	want := "Keep the old record and its data. To install a separate instance, choose an unused name and run: zalki plugins install FOLDER --as NEW_NAME"
	if err != nil || !strings.Contains(string(raw), want) || strings.Contains(string(raw), "plugins remove ") || strings.Contains(string(raw), "&&") {
		t.Fatal("OLD_RECORD_LIST_SUGGESTED_DESTRUCTIVE_RECOVERY", err, string(raw))
	}
	assertOldPluginRecoveryFiles(t, before)
	// An empty source folder refuses before install staging or publication.
	if raw, err := pluginOwnerLocal(t, store.Dir, "install", t.TempDir(), "--as", "new-ext"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("SEPARATE_INSTALL_DID_NOT_REFUSE_MISSING_MANIFEST", err, string(raw))
	}
	assertOldPluginRecoveryFiles(t, before)
	if _, err := os.Stat(filepath.Join(dir, "new-ext.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("REFUSED_SEPARATE_INSTALL_PUBLISHED_RECORD", err)
	}
}

func assertOldPluginRecoveryFiles(t *testing.T, before map[string][]byte) {
	t.Helper()
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("OLD_PLUGIN_RECOVERY_CHANGED_PRESERVED_FILES", err)
		}
	}
}
