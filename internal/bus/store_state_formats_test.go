package bus

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"

	"github.com/zalkilabs/zalki/internal/model"
)

func journalSnapshot(t *testing.T, store Store) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	if err := filepath.WalkDir(store.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(store.Dir, path)
		if err != nil {
			return err
		}
		snapshot[rel] = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func futureStateMarker(t *testing.T, store Store) {
	t.Helper()
	if err := writeJSON(filepath.Join(store.Dir, stateFormatFile), stateFormats{Schema: 1, Formats: map[string]int{"sessions": leaseSessionSchema + 1}}); err != nil {
		t.Fatal(err)
	}
}

func TestStateFormatMarkerIsLazyMonotonicAndPreservesExtensions(t *testing.T) {
	store := fixtureStore(t)
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(store.Dir, stateFormatFile)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatal("read created marker", err)
	}
	target, key := register(t, store, "cursor")
	marker, _, err := store.readStateFormats()
	if err != nil || marker.Formats["sessions"] != recordSchema {
		t.Fatal(marker, err)
	}
	addRecordExtension(t, markerPath)
	lease := armedLease(t, store, target.ID, key)
	marker, _, err = store.readStateFormats()
	if err != nil || marker.Formats["sessions"] != leaseSessionSchema {
		t.Fatal("lease format not declared", marker, err)
	}
	lease.Close(nil)
	marker, _, err = store.readStateFormats()
	if err != nil || marker.Formats["sessions"] != leaseSessionSchema {
		t.Fatal("format declaration downgraded", marker, err)
	}
	requireRecordExtension(t, markerPath)
}

func TestStateFormatFenceKeepsReadsAndRefusesEveryWriteWithoutChanges(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	from, key, target, targetKey := registerPair(t, store, "codex", "cursor")
	msg, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: target.ID, Body: "read only"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Watch(from.ID, key, target.ID); err != nil {
		t.Fatal(err)
	}
	futureStateMarker(t, store)
	before := journalSnapshot(t, store)
	if rows, err := store.List(); err != nil || len(rows) != 2 {
		t.Fatal("list blocked", err)
	}
	if rows, err := store.Discover(); err != nil || len(rows) != 2 {
		t.Fatal("discovery blocked", err)
	}
	if report, err := store.Diagnostics(t.Context()); err != nil || len(report.Sessions) != 2 {
		t.Fatal("doctor blocked", err)
	}
	query := MessageQuery{Actor: from.ID, Capability: key, Sent: true, Limit: 1}
	page, err := store.ReadMessages(query)
	if err != nil || len(page.Messages) != 1 {
		t.Fatal("paging blocked", err)
	}
	query.MessageID = msg.ID
	if _, err := store.ReadMessages(query); err != nil {
		t.Fatal("exact read blocked", err)
	}
	if _, err := store.Prune(true, nil); err != nil {
		t.Fatal("prune preview blocked", err)
	}
	for _, test := range []struct {
		name  string
		write func() error
	}{
		{"update", func() error { return store.Update(target.ID, model.StateBusy, false) }},
		{"retire", func() error { return store.Update(target.ID, model.StateRetired, true) }},
		{"register", func() error { _, _, err := store.Register("cursor", newID(), "/workspace", nil); return err }},
		{"task", func() error { _, err := service.SetTask(from.ID, key, "blocked"); return err }},
		{"send", func() error {
			_, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: target.ID, Body: "blocked"})
			return err
		}},
		{"ack", func() error { _, err := store.Ack(target.ID, targetKey, msg.ID); return err }},
		{"reply", func() error {
			_, err := service.Reply(t.Context(), ReplyRequest{Actor: target.ID, Capability: targetKey, MessageID: msg.ID, Body: "blocked"})
			return err
		}},
		{"watch", func() error { _, err := service.Watch(from.ID, key, target.ID); return err }},
		{"emit pending", func() error { _, err := store.EmitPending(target.ID); return err }},
		{"approval adoption", func() error { return service.releaseApprovalQueue(t.Context(), target.ID) }},
		{"prune", func() error { _, err := store.Prune(false, []string{target.ID}); return err }},
		{"lease", func() error { _, err := store.AcquireListenerLease(target.ID, targetKey); return err }},
		{"runtime", func() error { _, err := store.TrackRuntime("mcp", ""); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.write(); !IsUpgradeRequired(err) || model.ErrorCodeOf(err) != model.CodeUnsupportedSchema || !strings.Contains(err.Error(), "upgrade zalki") {
				t.Fatal("missing clear format fence", err)
			}
			if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
				t.Fatal("refused operation changed journal")
			}
		})
	}
	var diagnostic, output bytes.Buffer
	store.Logger = slog.New(slog.NewJSONHandler(&diagnostic, nil))
	service.Store = store
	for _, event := range []string{"SessionStart", "PostToolUse", "Stop"} {
		input, _ := json.Marshal(agent.HookInput{SessionID: model.NativeID(target), CWD: "/workspace", Event: event})
		if err := service.Hook(t.Context(), "codex", bytes.NewReader(input), &output); err != nil {
			t.Fatal("hook broke client", err)
		}
	}
	if output.Len() != 0 || strings.Count(diagnostic.String(), "hook.upgrade_required") != 3 {
		t.Fatal("hook did not degrade to logged no-op", output.String(), diagnostic.String())
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("read or hook changed fenced journal")
	}
}

func TestFutureMarkerDefersReplyRecoveryAndRetainsUnknownIntentFields(t *testing.T) {
	store := fixtureStore(t)
	from, _, target, _ := registerPair(t, store, "codex", "cursor")
	parent := Message{ID: newID(), From: from.ID, To: target.ID, Body: "question", Status: model.StatusSubmitted, TargetGeneration: target.Generation}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	reply := Message{Schema: 1, ID: replyID(target.ID, parent.ID, "answer"), From: target.ID, To: from.ID, ReplyTo: parent.ID, Body: "answer", Status: model.StatusAttempting, TargetGeneration: from.Generation}
	tx := replyTransaction{Schema: 1, ParentID: parent.ID, AcknowledgedAt: model.Timestamp(), Reply: reply}
	txPath := filepath.Join(store.Dir, "transactions", reply.ID+".json")
	if err := os.MkdirAll(filepath.Dir(txPath), 0700); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(tx)
	var intent map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &intent); err != nil {
		t.Fatal(err)
	}
	var outgoing map[string]json.RawMessage
	if err := json.Unmarshal(intent["reply"], &outgoing); err != nil {
		t.Fatal(err)
	}
	outgoing["future_extension"] = futureExtension
	intent["reply"], _ = json.Marshal(outgoing)
	if err := writeJSON(txPath, intent); err != nil {
		t.Fatal(err)
	}
	futureStateMarker(t, store)
	before := journalSnapshot(t, store)
	if _, err := store.List(); err != nil {
		t.Fatal("fenced read attempted recovery", err)
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("read replayed durable intent under future marker")
	}
	if err := writeJSON(filepath.Join(store.Dir, stateFormatFile), stateFormats{Schema: 1, Formats: map[string]int{"sessions": leaseSessionSchema}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err != nil {
		t.Fatal("compatible recovery failed", err)
	}
	requireRecordExtension(t, filepath.Join(store.Dir, "messages", reply.ID+".json"))
	recovered, err := store.message(parent.ID)
	if err != nil || recovered.AcknowledgedAt == "" {
		t.Fatal("recovery lost parent ACK", err)
	}
	if _, err := os.Stat(txPath); !os.IsNotExist(err) {
		t.Fatal("completed intent retained", err)
	}
}

func TestMalformedOrUnknownFormatMarkersFailWritesButKeepReads(t *testing.T) {
	for _, raw := range []string{`{`, `null`, `{}`, `{"schema":1}`, `{"formats":{}}`, `[]`, `"marker"`, `{"schema":1,"formats":null}`, `{"schema":1,"formats":[]}`, `{"schema":null,"formats":{}}`, `{"schema":2,"formats":{}}`, `{"schema":1,"formats":{"future_queue":1}}`, `{"schema":1,"formats":{"sessions":0}}`} {
		t.Run(raw, func(t *testing.T) {
			store := fixtureStore(t)
			target, _ := register(t, store, "codex")
			path := filepath.Join(store.Dir, stateFormatFile)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.List(); err != nil {
				t.Fatal("marker blocked known reads", err)
			}
			if err := store.Update(target.ID, model.StateBusy, false); !IsUpgradeRequired(err) {
				t.Fatal("invalid marker permitted write", err)
			}
			if after, _ := os.ReadFile(path); string(after) != raw {
				t.Fatal("unsupported marker was rewritten")
			}
		})
	}
}

func TestExplicitEmptyFormatMarkerAllowsWrites(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "codex")
	path := filepath.Join(store.Dir, stateFormatFile)
	if err := os.WriteFile(path, []byte(`{"schema":1,"formats":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(target.ID, model.StateBusy, false); err != nil {
		t.Fatal("explicit valid empty marker fenced writes", err)
	}
	marker, _, err := store.readStateFormats()
	if err != nil || marker.Schema != 1 || marker.Formats["sessions"] != recordSchema {
		t.Fatal("empty marker did not gain written format", marker, err)
	}
}
