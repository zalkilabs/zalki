package bus

import (
	"bytes"
	"context"
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

func TestNativeNamesRefreshWithoutChangingIdentity(t *testing.T) {
	root := nameFixture(t)
	store := fixtureStore(t)
	for _, kind := range []string{"codex", "claude", "cursor"} {
		t.Run(kind, func(t *testing.T) {
			session, key := register(t, store, kind)
			for _, name := range []string{"First native title", "Renamed native title"} {
				nativeNameFixture(t, root, session, name)
				items, err := store.List()
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, item := range items {
					if item.ID == session.ID {
						found = true
						if item.Name != name || item.NameSource == "" || item.KeyHash != "" || item.Generation != session.Generation {
							t.Fatalf("bad discovery: %+v", item)
						}
					}
				}
				if !found {
					t.Fatal("missing session")
				}
				if _, err := store.authenticate(session.ID, key); err != nil {
					t.Fatal("discovery changed capability", err)
				}
			}
			nativeNameFixture(t, root, session, "bad\nname")
			items, _ := store.List()
			for _, item := range items {
				if item.ID == session.ID && item.Name != "" {
					t.Fatal("accepted control character in native name")
				}
			}
		})
	}
}

func TestNameResolutionAmbiguityAndRetirement(t *testing.T) {
	root := nameFixture(t)
	store := fixtureStore(t)
	sender, key := register(t, store, "cursor")
	a, _ := register(t, store, "codex")
	b, _ := register(t, store, "cursor")
	for _, v := range []Session{a, b} {
		nativeNameFixture(t, root, v, "reviewer")
	}
	service := Service{Store: store, Deliver: func(_ context.Context, msg Message, target Session) Receipt {
		if msg.To != a.ID || target.ID != a.ID {
			t.Fatal("transport received a name instead of the resolved UUID")
		}
		return Receipt{Status: "submitted"}
	}}
	for _, name := range []string{"reviewer", "Reviewer", "missing"} {
		_, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: name, MessageID: newID(), Body: "hello"})
		if err == nil {
			t.Fatalf("accepted ambiguous/unknown name %q", name)
		}
		if name == "reviewer" && (!strings.Contains(err.Error(), a.ID) || !strings.Contains(err.Error(), b.ID)) {
			t.Fatalf("ambiguity did not identify candidates: %v", err)
		}
	}
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: "codex:reviewer", MessageID: newID(), Body: "hello"})
	if err != nil || msg.To != a.ID || msg.RequestedTo != "codex:reviewer" {
		t.Fatalf("qualified name failed: %+v %v", msg, err)
	}
	c, _ := register(t, store, "codex")
	nativeNameFixture(t, root, c, "reviewer")
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: "codex:reviewer", MessageID: newID(), Body: "hello"}); err == nil {
		t.Fatal("accepted duplicate names within a kind")
	}
	if err := store.Update(c.ID, "retired", true); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(b.ID, "retired", true); err != nil {
		t.Fatal(err)
	}
	msg, err = service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: "reviewer", MessageID: newID(), Body: "hello"})
	if err != nil || msg.To != a.ID {
		t.Fatalf("retired session caused false ambiguity: %+v %v", msg, err)
	}
	// Even when a native name looks exactly like an ID, the exact ID wins.
	nativeNameFixture(t, root, sender, a.ID)
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: a.ID, MessageID: newID(), Body: "hello"}); err != nil {
		t.Fatal(err)
	}
}

func TestNameRetryRemainsBoundAfterRenameAndReassignment(t *testing.T) {
	root := nameFixture(t)
	store := fixtureStore(t)
	sender, key := register(t, store, "cursor")
	a, _ := register(t, store, "codex")
	b, _ := register(t, store, "codex")
	nativeNameFixture(t, root, a, "reviewer")
	calls := 0
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		calls++
		return Receipt{Status: "unknown_after_timeout"}
	}}
	id := newID()
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: "reviewer", MessageID: id, Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	nativeNameFixture(t, root, a, "renamed")
	nativeNameFixture(t, root, b, "reviewer")
	if err := store.Update(a.ID, "retired", true); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"reviewer", a.ID} {
		msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: address, MessageID: id, Body: "hello"})
		if err != nil || msg.To != a.ID || calls != 1 {
			t.Fatalf("retry rerouted/resubmitted: %+v %v calls=%d", msg, err, calls)
		}
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: b.ID, MessageID: id, Body: "hello"}); err == nil {
		t.Fatal("rebound a reserved message UUID to a different session")
	}
}

func TestNamesPreserveReplyAndCapabilityScope(t *testing.T) {
	root := nameFixture(t)
	store := fixtureStore(t)
	a, aKey := register(t, store, "cursor")
	b, bKey := register(t, store, "cursor")
	c, _ := register(t, store, "cursor")
	for _, pair := range []struct {
		v    Session
		name string
	}{{a, "sender"}, {b, "receiver"}, {c, "other"}} {
		nativeNameFixture(t, root, pair.v, pair.name)
	}
	service := Service{Store: store}
	parent, err := service.Send(context.Background(), SendRequest{Actor: a.ID, Capability: aKey, To: "receiver", MessageID: newID(), Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: b.ID, Capability: bKey, To: "other", MessageID: newID(), Body: "reply", ReplyTo: parent.ID}); err == nil {
		t.Fatal("name bypassed reply scope")
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: b.ID, Capability: aKey, To: "sender", MessageID: newID(), Body: "reply", ReplyTo: parent.ID}); err == nil {
		t.Fatal("name bypassed authentication")
	}
	msg, err := service.Send(context.Background(), SendRequest{Actor: b.ID, Capability: bKey, To: "sender", MessageID: newID(), Body: "reply", ReplyTo: parent.ID})
	if err != nil || msg.To != a.ID {
		t.Fatalf("reply by unique name failed: %+v %v", msg, err)
	}
}

func TestClaudeHookTitleAndMissingNativeMetadata(t *testing.T) {
	nameFixture(t)
	store := fixtureStore(t)
	session, _ := register(t, store, "claude")
	service := Service{Store: store}
	input := `{"session_id":"` + strings.TrimPrefix(session.ID, "claude:") + `","hook_event_name":"Stop","session_title":"reviewer"}`
	if err := service.Hook("claude", strings.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || items[0].Name != "reviewer" || items[0].NameSource != "claude_session_title_hook" {
		t.Fatalf("missing hook title fallback: %+v %v", items, err)
	}
	input = strings.ReplaceAll(input, `"reviewer"`, `""`)
	if err := service.Hook("claude", strings.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	items, err = store.List()
	if err != nil || items[0].Name != "" {
		t.Fatal("did not clear explicit empty title", err)
	}
}

func TestConflictingOrUnsupportedMetadataHasNoName(t *testing.T) {
	root := nameFixture(t)
	store := fixtureStore(t)
	cursor, _ := register(t, store, "cursor")
	claude, _ := register(t, store, "claude")
	codex, _ := register(t, store, "codex")
	for _, v := range []Session{cursor, claude, codex} {
		nativeNameFixture(t, root, v, "reviewer")
	}
	cursorID := strings.TrimPrefix(cursor.ID, "cursor:")
	other := filepath.Join(root, "cursor", "chats", "other-workspace", cursorID, "meta.json")
	if err := os.MkdirAll(filepath.Dir(other), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte(`{"schemaVersion":1,"title":"conflicting"}`), 0600); err != nil {
		t.Fatal(err)
	}
	claudeID := strings.TrimPrefix(claude.ID, "claude:")
	if err := writeJSON(filepath.Join(root, "claude", "sessions", "another-process.json"), map[string]string{"sessionId": claudeID, "name": "conflicting"}); err != nil {
		t.Fatal(err)
	}
	// An interrupted index append must not bring the previous label back.
	f, err := os.OpenFile(filepath.Join(root, "codex", "session_index.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"id":`)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Name != "" {
			t.Fatalf("exposed ambiguous metadata as a name: %+v", item)
		}
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cursor", "chats", "workspace-hash", cursorID, "meta.json")
	if err := os.WriteFile(path, []byte(`{"schemaVersion":99,"title":"reviewer"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := cursorName(filepath.Join(root, "cursor"), cursorID); got != "" {
		t.Fatalf("accepted unsupported metadata version: %q", got)
	}
}

func TestNamesUseOwningSessionConfigDirectory(t *testing.T) {
	root := nameFixture(t)
	store := fixtureStore(t)
	sessions := map[string]Session{}
	var actor, key string
	for _, kind := range []string{"codex", "claude", "cursor"} {
		v, capability := register(t, store, kind)
		sessions[kind] = v
		nativeNameFixture(t, root, v, kind+" reviewer")
		if kind == "cursor" {
			actor, key = v.ID, capability
		}
	}
	// A different sender MCP can inherit unrelated config roots, or no XDG env.
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, filepath.Join(t.TempDir(), "unrelated"))
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	items, err := store.List()
	if err != nil || len(items) != 3 {
		t.Fatalf("discovery failed: %+v %v", items, err)
	}
	for _, item := range items {
		if item.Name != item.Kind+" reviewer" || item.NativeConfigDir != "" {
			t.Fatalf("sender environment changed receiver name: %+v", item)
		}
	}
	msg, target, fresh, err := store.ReserveMessage(SendRequest{Actor: actor, Capability: key, To: "codex:codex reviewer", MessageID: newID(), Body: "hello"})
	if err != nil || !fresh || msg.To != sessions["codex"].ID || target.ID != msg.To {
		t.Fatalf("named resolution used the sender's config root: %+v %v", msg, err)
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
