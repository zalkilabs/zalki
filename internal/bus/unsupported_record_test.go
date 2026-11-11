package bus

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
)

// With one build, a record with another schema is a damaged file. These tests
// pin how each path treats one: skip and count, warn and keep going, or clean up.

func writeRawMessage(t *testing.T, store Store, id string, record map[string]any) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", id+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// A hook skips a marked message file with a newer schema, counts it, and
// still delivers the readable message. The inbox list counts it too.
func TestHookSkipsAndCountsAMessageWithAnUnsupportedSchema(t *testing.T) {
	f := newOpenFixture(t)
	bad := newID()
	markOpen(t, f.store, bad)
	writeRawMessage(t, f.store, bad, map[string]any{"schema": 2, "id": bad, "to": f.target.ID})
	pending := f.queue(t, "pending", time.Now())
	output := capture(&f.store)
	text, err := f.store.EmitPending(f.target.ID)
	if err != nil || !strings.Contains(text, pending.ID) {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	requireOneSkipRecord(t, output, f.target.ID, bad, 1)
	batch, err := f.store.ReadMessages(MessageQuery{Actor: f.target.ID, Capability: f.targetKey})
	if err != nil || batch.Unreadable != 1 {
		t.Fatalf("inbox unreadable = %d, %v", batch.Unreadable, err)
	}
}

// A message file with no schema field is refused on read, like any other
// schema that is not the current one.
func TestAMessageWithoutASchemaIsUnreadable(t *testing.T) {
	f := newOpenFixture(t)
	bad := newID()
	writeRawMessage(t, f.store, bad, map[string]any{"id": bad, "to": f.target.ID})
	if _, err := f.store.message(bad); err == nil {
		t.Fatal("a message without a schema was read")
	}
	batch, err := f.store.ReadMessages(MessageQuery{Actor: f.target.ID, Capability: f.targetKey})
	if err != nil || batch.Unreadable != 1 {
		t.Fatalf("inbox unreadable = %d, %v", batch.Unreadable, err)
	}
}

// A hook whose own session record has an unsupported schema cannot skip it. It
// logs one warn record with allowlisted keys only and returns success, so the
// owner's agent is never broken.
func TestHookWithAnUnsupportedOwnSessionWarnsAndSucceeds(t *testing.T) {
	service := newTestService(t)
	target, _ := register(t, service.Store, "codex")
	setSessionSchema(t, service.Store, target.ID, 9)
	var diagnostic, output bytes.Buffer
	service.Store.Logger = slog.New(slog.NewJSONHandler(&diagnostic, &slog.HandlerOptions{Level: slog.LevelDebug}))
	input, _ := json.Marshal(agent.HookInput{SessionID: target.NativeID, CWD: "/workspace", Event: "Stop"})
	if err := service.Hook(t.Context(), "codex", bytes.NewReader(input), &output); err != nil {
		t.Fatal("hook broke the agent", err)
	}
	if output.Len() != 0 {
		t.Fatalf("hook wrote output: %q", output.String())
	}
	requireOneUnsupportedRecord(t, diagnostic.String(), service.Store.Dir)
}

// setSessionSchema rewrites the schema field of a session record on disk.
func setSessionSchema(t *testing.T, store Store, id string, schema int) {
	t.Helper()
	path, err := store.sessionPath(id)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &record) != nil {
		t.Fatal(err)
	}
	record["schema"] = schema
	if raw, err = json.Marshal(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// requireOneUnsupportedRecord checks that the log has exactly one
// hook.record_unsupported warn record, with allowlisted keys only and no state
// path or error text.
func requireOneUnsupportedRecord(t *testing.T, log, stateDir string) {
	t.Helper()
	allowed := []string{"time", "level", "msg", "agent", "actor", "hook_event", "error_kind", "error_code"}
	found := 0
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["msg"] != "hook.record_unsupported" {
			continue
		}
		found++
		if entry["level"] != "WARN" || entry["error_code"] != "unsupported_schema" {
			t.Fatalf("record = %v", entry)
		}
		for key := range entry {
			if !slices.Contains(allowed, key) {
				t.Fatalf("record has key %q outside the allowlist: %v", key, entry)
			}
		}
	}
	if found != 1 || strings.Contains(log, stateDir) || strings.Contains(log, "schema 9") {
		t.Fatalf("want one record without paths or error text, got %d: %s", found, log)
	}
}
