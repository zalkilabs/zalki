package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestListKeepsHealthySessionsAndWarnsOnceForSkippedRecords(t *testing.T) {
	for _, key := range []string{"CODEX_HOME", "CURSOR_CONFIG_DIR", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	home := t.TempDir()
	state := filepath.Join(home, "state")
	store := bus.Store{Dir: state, Registry: adapters.Builtin()}
	healthy, _, err := store.Register("cursor", "00000000-0000-4000-8000-000000000061", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	future, _, err := store.Register("cursor", "00000000-0000-4000-8000-000000000062", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "sessions", strings.ReplaceAll(future.ID, ":", "_")+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record["schema"] = json.RawMessage(`99`)
	raw, _ = json.Marshal(record)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	output, warning, err := execute(t, home, "", "list", "--state", state)
	if err != nil || !strings.Contains(output, healthy.ID) || strings.Contains(output, future.ID) || !strings.Contains(warning, "skipped 1") || strings.Count(warning, "\n") != 1 {
		t.Fatal("list lost output or warning", output, warning, err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(raw, after) {
		t.Fatal("list changed skipped record")
	}
}
