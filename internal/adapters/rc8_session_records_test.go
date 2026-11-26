package adapters_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The files in testdata/rc8_sessions were written by the rc.8 build (tag
// v0.1.0-rc.8, one session per client, fixed IDs and config roots). The current
// build must read them as they are: no clean install, no migration.
func TestSessionRecordsWrittenByRC8StillLoad(t *testing.T) {
	store := adapterTestStore(t)
	dir := filepath.Join(store.Dir(), "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"claude": "claude:00000000-0000-4000-8000-0000000c1a01",
		"codex":  "codex:00000000-0000-4000-8000-0000000c0d02",
		"cursor": "cursor:00000000-0000-4000-8000-0000000c5003",
	}
	for kind, id := range want {
		raw, err := os.ReadFile(filepath.Join("testdata", "rc8_sessions", kind+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, strings.ReplaceAll(id, ":", "_")+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", "/fixture/home/.codex")
	t.Setenv("CLAUDE_CONFIG_DIR", "/fixture/home/.claude")
	t.Setenv("CURSOR_CONFIG_DIR", "/fixture/home/.cursor")
	rows, skipped, err := store.ListWithWarnings()
	if err != nil || skipped != 0 || len(rows) != len(want) {
		t.Fatalf("rc.8 session records: %d rows, %d skipped, %v; want %d rows and none skipped", len(rows), skipped, err, len(want))
	}
	for _, row := range rows {
		if row.ID != want[row.Kind] {
			t.Fatalf("session %s has kind %s; want ID %s", row.ID, row.Kind, want[row.Kind])
		}
	}
}
