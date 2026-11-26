package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A record that cannot be read (here: the old inbox_from/inbox_read grant
// keys) is refused with the one plain error that names the fix; list shows it
// as unreadable with the same text; explicit remove still deletes it.
func TestPluginListAndUnreadableRecord(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	exe := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "ext", "--exec", exe}, {"configure", "ext", "--scope", "all"}, {"grant", "ext", "--status", "--receive"}} {
		if raw, err := pluginOwnerLocal(t, store.Dir(), args...); err != nil {
			t.Fatal(args, string(raw), err)
		}
	}
	raw, err := pluginOwnerLocal(t, store.Dir(), "list")
	if err != nil || !strings.Contains(string(raw), "Manual plugins (no installation receipt)\n  ext  unknown version  disabled; scope all; 0 eligible in the session snapshot; SEND 0; READ 0; installed by unknown (no installation receipt)") {
		t.Fatalf("list: %q %v", raw, err)
	}
	writeOldPluginRecord(t, filepath.Join(store.Dir(), "plugins", "ext.json"))
	fix := "plugin record ext cannot be read; remove it with: zalki plugins remove ext"
	assertOldRecordRefused(t, store.Dir(), fix)
	if raw, err := pluginOwnerLocal(t, store.Dir(), "list"); err != nil || !strings.Contains(string(raw), "Unreadable plugin records\n  ext  "+fix) {
		t.Fatalf("list of an old record: %q %v", raw, err)
	}
	assertUnreadableRecordLogsOnlyItsReason(t, store.Dir(), "decode")
	if raw, err := pluginOwnerLocal(t, store.Dir(), "remove", "ext"); err != nil {
		t.Fatal("remove of an old record", string(raw), err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir(), "list"); err != nil || string(raw) != "No plugins are installed.\n" {
		t.Fatalf("list after remove: %q %v", raw, err)
	}
}

// assertUnreadableRecordLogsOnlyItsReason checks the command.failed log line of
// a refused record: one fixed reason name, no error text, no path.
func assertUnreadableRecordLogsOnlyItsReason(t *testing.T, state, reason string) {
	t.Helper()
	_, diagnostic, err := execute(t, t.TempDir(), "", "--state", state, "--log-level=debug", "--log-file=-", "plugins", "status", "ext")
	log := diagnostic
	if err == nil || !strings.Contains(log, `"msg":"command.failed"`) || !strings.Contains(log, `"reason":"`+reason+`"`) {
		t.Fatalf("command.failed has no reason %q: %q %v", reason, log, err)
	}
	if strings.Contains(log, "cannot be read") || strings.Contains(log, state) {
		t.Fatalf("command.failed carries error text or a path: %q", log)
	}
}

// writeOldPluginRecord turns a record into a shape the current build does not
// read: no scope, a session list and inbox_from/inbox_read in place of receive.
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
	// The shape before rule scope: a session list and the old grant keys.
	for _, key := range []string{"scope", "access", "revision"} {
		delete(fields, key)
	}
	fields["sessions"] = []string{}
	fields["capabilities"] = map[string]any{"inbox_from": []string{}, "inbox_read": true}
	old, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertOldRecordRefused(t *testing.T, state, fix string) {
	t.Helper()
	for _, args := range [][]string{{"status", "ext"}, {"enable", "ext"}, {"grant", "ext", "--status", "--receive"}} {
		if raw, err := pluginOwnerLocal(t, state, args...); err == nil || err.Error() != fix {
			t.Fatalf("%v on an old record: %v %s", args, err, raw)
		}
	}
}
