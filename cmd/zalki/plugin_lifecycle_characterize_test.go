package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pluginStateFiles lists plugin state names with UUIDs masked, so a lifecycle
// step can be compared by shape. Lease files persist by design.
func pluginStateFiles(t *testing.T, state string) []string {
	t.Helper()
	var names []string
	root := filepath.Join(state, "plugins")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "id-") {
			rel = "id-UUID.json"
		}
		if entry.IsDir() {
			rel += "/"
		}
		names = append(names, rel)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return names
}

func pluginRecordKeys(t *testing.T, state, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, "plugins", name+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &fields) != nil {
		t.Fatal("plugin record unreadable", err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Pristine characterization of the manual add/configure/grant/enable/status/
// disable/revoke/remove lifecycle that plugins install must not change.
func TestPluginLifecycleCharacterization(t *testing.T) {
	store, session := pluginOwnerFixture(t)
	file := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		args  []string
		files string
		keys  string
	}{
		{[]string{"add", "ext", "--exec", file}, "ext.data/ ext.json ext.lock id-UUID.json", "enabled id launch name position sessions"},
		{[]string{"configure", "ext", "--session", session.ID}, "ext.data/ ext.json ext.lock id-UUID.json", "enabled id launch name position sessions"},
		{[]string{"grant", "ext", "--read", session.ID}, "ext.data/ ext.json ext.lock id-UUID.json", "capabilities enabled id launch name position sessions"},
		{[]string{"enable", "ext"}, "ext.data/ ext.json ext.lock id-UUID.json", "capabilities enabled gap_id gap_reason id launch name position sessions"},
		{[]string{"status", "ext"}, "ext.data/ ext.json ext.lock id-UUID.json", "capabilities enabled gap_id gap_reason id launch name position sessions"},
		{[]string{"disable", "ext"}, "ext.data/ ext.json ext.lock id-UUID.json", "capabilities enabled gap_id gap_reason id launch name position sessions"},
		{[]string{"revoke", "ext"}, "ext.data/ ext.json ext.lock id-UUID.json", "enabled gap_id gap_reason id launch name position sessions"},
		{[]string{"configure", "ext", "--session", session.ID}, "ext.data/ ext.json ext.lock id-UUID.json", "enabled id launch name position sessions"},
		{[]string{"remove", "ext"}, "ext.lock", ""},
	}
	for _, step := range steps {
		if raw, err := pluginOwnerLocal(t, store.Dir, step.args...); err != nil {
			t.Fatal(step.args, string(raw), err)
		}
		files := strings.Join(pluginStateFiles(t, store.Dir), " ")
		keys := strings.Join(pluginRecordKeys(t, store.Dir, "ext"), " ")
		if files != step.files || keys != step.keys {
			t.Fatalf("%v changed shape:\nfiles %q\nkeys  %q", step.args, files, keys)
		}
	}
	// Enabled installations refuse grant changes, configure and removal.
	for _, args := range [][]string{{"add", "ext", "--exec", file}, {"configure", "ext", "--session", session.ID}, {"enable", "ext"}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, args...); err != nil {
			t.Fatal(args, string(raw), err)
		}
	}
	for _, args := range [][]string{{"configure", "ext", "--session", session.ID}, {"grant", "ext", "--read", session.ID}, {"remove", "ext"}, {"add", "ext", "--exec", file}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, args...); err == nil {
			t.Fatal("enabled installation accepted", args, string(raw))
		}
	}
}
