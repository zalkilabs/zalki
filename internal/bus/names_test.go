package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nameFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(root, "cursor"))
	return root
}

func nativeNameFixture(t *testing.T, root string, session Session, name string) {
	t.Helper()
	id := strings.TrimPrefix(session.ID, session.Kind+":")
	var path string
	var row any
	switch session.Kind {
	case "codex":
		path = filepath.Join(root, "codex", "session_index.jsonl")
		row = map[string]string{"id": id, "thread_name": name}
	case "claude":
		path = filepath.Join(root, "claude", "sessions", id+".json")
		row = map[string]string{"sessionId": id, "name": name, "nameSource": "custom"}
	case "cursor":
		path = filepath.Join(root, "cursor", "chats", "workspace-hash", id, "meta.json")
		row = map[string]any{"schemaVersion": 1, "title": name}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(row)
	if session.Kind == "codex" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyCursorNameLookupWithoutXDGEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CURSOR_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	store := fixtureStore(t)
	session, _ := register(t, store, "cursor")
	// Simulate an existing record created before config-root capture existed.
	session.NativeConfigDir = ""
	if err := store.locked(func() error { return store.saveSession(session) }); err != nil {
		t.Fatal(err)
	}
	nativeNameFixture(t, filepath.Join(home, ".config"), session, "Zalki ID Report")
	items, err := store.List()
	if err != nil || len(items) != 1 || items[0].Name != "Zalki ID Report" {
		t.Fatalf("lost legacy Cursor title without XDG env: %+v %v", items, err)
	}
	// Different labels for the UUID across standard locations are not guessed.
	path := filepath.Join(home, ".cursor", "chats", "other", strings.TrimPrefix(session.ID, "cursor:"), "meta.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, map[string]any{"schemaVersion": 1, "title": "Conflicting title"}); err != nil {
		t.Fatal(err)
	}
	items, err = store.List()
	if err != nil || items[0].Name != "" {
		t.Fatalf("guessed between conflicting legacy roots: %+v %v", items, err)
	}
}

func TestHookBackfillsNativeConfigWithoutReregistering(t *testing.T) {
	root := nameFixture(t)
	store := fixtureStore(t)
	session, key := register(t, store, "cursor")
	session.NativeConfigDir = ""
	if err := store.locked(func() error { return store.saveSession(session) }); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	input := `{"conversation_id":"` + strings.TrimPrefix(session.ID, "cursor:") + `","hook_event_name":"stop","status":"aborted"}`
	if err := service.Hook("cursor", strings.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	nativeNameFixture(t, root, session, "Captured title")
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(t.TempDir(), "sender"))
	items, err := store.List()
	if err != nil || items[0].Name != "Captured title" || items[0].Generation != session.Generation {
		t.Fatalf("hook did not preserve identity and capture root: %+v %v", items, err)
	}
	if _, err := store.authenticate(session.ID, key); err != nil {
		t.Fatal("backfill changed capability", err)
	}
}
