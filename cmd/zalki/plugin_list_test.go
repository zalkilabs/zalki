package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An old record (inbox_from/inbox_read) is refused with the exact fix; list
// still shows it, and remove deletes it.
func TestPluginListAndOldRecord(t *testing.T) {
	store, session := pluginOwnerFixture(t)
	exe := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "ext", "--exec", exe}, {"configure", "ext", "--session", session.ID}, {"grant", "ext", "--receive", session.ID}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, args...); err != nil {
			t.Fatal(args, string(raw), err)
		}
	}
	raw, err := pluginOwnerLocal(t, store.Dir, "list")
	if err != nil || string(raw) != "ext  disabled  changes from 1, receive from 1, send to 0, read from 0 session(s)\n" {
		t.Fatalf("list: %q %v", raw, err)
	}
	writeOldPluginRecord(t, filepath.Join(store.Dir, "plugins", "ext.json"))
	fix := "ext: plugin record from an older Zalki. Run: zalki plugins remove ext && zalki plugins install FOLDER"
	assertOldRecordRefused(t, store.Dir, fix, session.ID)
	if raw, err := pluginOwnerLocal(t, store.Dir, "list"); err != nil || !strings.HasPrefix(string(raw), "ext  old record, remove it: "+fix) {
		t.Fatalf("list of an old record: %q %v", raw, err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "remove", "ext"); err != nil {
		t.Fatal("remove of an old record", string(raw), err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "list"); err != nil || string(raw) != "No plugins are installed.\n" {
		t.Fatalf("list after remove: %q %v", raw, err)
	}
}

// writeOldPluginRecord turns a record into the shape an older Zalki wrote:
// inbox_from and inbox_read in place of receive.
func writeOldPluginRecord(t *testing.T, path string) {
	t.Helper()
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(record, &fields); err != nil {
		t.Fatal(err)
	}
	c := fields["capabilities"].(map[string]any)
	c["inbox_from"], c["inbox_read"] = c["receive"], true
	delete(c, "receive")
	old, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertOldRecordRefused(t *testing.T, state, fix, session string) {
	t.Helper()
	for _, args := range [][]string{{"status", "ext"}, {"enable", "ext"}, {"grant", "ext", "--receive", session}} {
		if raw, err := pluginOwnerLocal(t, state, args...); err == nil || err.Error() != fix {
			t.Fatalf("%v on an old record: %v %s", args, err, raw)
		}
	}
}
