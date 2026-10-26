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

func TestNativeIdentityIsIndependentOfJournalAddress(t *testing.T) {
	store := fixtureStore(t)
	registry, err := NewRegistry(fixtureAdapter{kind: "fourth", inbox: InboxNative})
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
	native := "workspace/branch:conversation-42"
	first, key, err := store.Register("fourth", native, "/workspace", nil)
	if err != nil || first.NativeID != native {
		t.Fatal(first, err)
	}
	path, err := store.sessionPath(first.ID)
	if err != nil || strings.Contains(path, "conversation") {
		t.Fatal("native ID became a journal path", path, err)
	}
	if _, err := UUID(strings.TrimPrefix(first.ID, "fourth:")); err != nil {
		t.Fatal(err)
	}
	second, _, err := store.Register("fourth", native, "/workspace", nil)
	if err != nil || second.ID != first.ID || second.Generation == first.Generation {
		t.Fatal("registration lost its stable address or generation fence", err)
	}
	if _, err := store.authenticate(first.ID, key); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("old capability survived registration", err)
	}
	from, fromKey, err := store.Register("fourth", "another-native-id", "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Deliver: func(_ context.Context, _ Message, target Session) Receipt {
		if NativeID(target) != native || target.ID != first.ID || target.KeyHash != "" {
			t.Error("adapter did not receive the original native identity with credentials removed")
		}
		return Receipt{Status: StatusSubmitted}
	}}
	if _, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: fromKey, To: first.ID, MessageID: newID(), Body: "ping"}); err != nil {
		t.Fatal(err)
	}
	// A UUID-shaped native ID cannot overwrite a mapped non-UUID identity.
	if _, _, err := store.Register("fourth", strings.TrimPrefix(first.ID, "fourth:"), "/workspace", nil); ErrorCodeOf(err) != CodeConflict {
		t.Fatal("native identity collision accepted", err)
	}
	input, _ := json.Marshal(HookInput{SessionID: native, CWD: "/workspace", Event: "end"})
	if err := service.Hook("fourth", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(first.ID)
	if err != nil || !current.Retired {
		t.Fatal("native hook did not resolve the mapped address", err)
	}
}

func TestNativeIdentityValidationAndUUIDCompatibility(t *testing.T) {
	store := fixtureStore(t)
	id := newID()
	v, _, err := store.Register("codex", strings.ToUpper(id), "/workspace", nil)
	if err != nil || v.ID != "codex:"+id || v.NativeID != id {
		t.Fatal("legacy UUID address changed", err)
	}
	for _, invalid := range []string{"", "id\nforged", "id\x00", string([]byte{255}), strings.Repeat("a", 1025)} {
		if _, _, err := store.Register("codex", invalid, "/workspace", nil); ErrorCodeOf(err) != CodeInvalidIdentity {
			t.Fatal("invalid native ID accepted", err)
		}
	}
}

func TestOpaqueAdapterStateSurvivesCompaction(t *testing.T) {
	store := fixtureStore(t)
	registry, err := NewRegistry(fixtureAdapter{kind: "fourth", inbox: InboxNative})
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
	v, oldKey, err := store.Register("fourth", "native-conversation", "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	v.AdapterState = json.RawMessage(`{"schema":1,"client_field":"keep"}`)
	v.State = StateBusy
	if err := store.saveSession(v); err != nil {
		t.Fatal(err)
	}
	next, key, err := store.register(v.Kind, v.NativeID, v.CWD, nil, true)
	if err != nil || next.Generation != v.Generation || next.State != v.State || !equalJSON(next.AdapterState, v.AdapterState) {
		t.Fatal("compaction lost adapter-owned state", err)
	}
	if _, err := store.authenticate(v.ID, oldKey); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("compaction retained the old credential", err)
	}
	if _, err := store.authenticate(v.ID, key); err != nil {
		t.Fatal("compaction did not install the new credential", err)
	}
	if err := store.Update(v.ID, StateRetired, true); err != nil {
		t.Fatal(err)
	}
	replacement, _, err := store.register(v.Kind, v.NativeID, v.CWD, nil, true)
	if err != nil || replacement.Generation == next.Generation || replacement.Retired {
		t.Fatal("compaction reused a retired generation", err)
	}
}

func TestLegacyRecordsMigrateOnWrite(t *testing.T) {
	store := fixtureStore(t)
	v, key := register(t, store, "cursor")
	path, _ := store.sessionPath(v.ID)
	var legacy map[string]any
	if err := readJSON(path, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "schema")
	delete(legacy, "native_id")
	delete(legacy, "adapter_state")
	legacy["cursor_wake"] = Endpoint{Path: "/fixture/wake"}
	legacy["cursor_recovery_attempted"], legacy["cursor_recovery_suppressed"] = true, true
	if err := writeJSON(path, legacy); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	loaded, err := store.session(v.ID)
	state := cursorState(loaded)
	if err != nil || NativeID(loaded) != NativeID(v) || state.Wake == nil || !state.RecoveryAttempted || !state.RecoverySuppressed || !loaded.Receive.IdleWake {
		t.Fatal("legacy Cursor state lost", err)
	}
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("discovery rewrote a legacy record")
	}
	if err := store.Update(v.ID, StateBusy, false); err != nil {
		t.Fatal(err)
	}
	var upgraded map[string]any
	if err := readJSON(path, &upgraded); err != nil {
		t.Fatal(err)
	}
	if upgraded["schema"] != float64(recordSchema) || upgraded["native_id"] != NativeID(v) || upgraded["adapter_state"] == nil || upgraded["cursor_wake"] != nil || upgraded["cursor_recovery_attempted"] != nil {
		t.Fatal("adapter state was not migrated", upgraded)
	}
	from, _ := register(t, store, "codex")
	msg := Message{ID: newID(), From: from.ID, To: v.ID, Body: "ping", Status: StatusSubmitted}
	messagePath := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if err := writeJSON(messagePath, msg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ack(v.ID, key, msg.ID); err != nil {
		t.Fatal(err)
	}
	var migrated Message
	if err := readJSON(messagePath, &migrated); err != nil || migrated.Schema != recordSchema || migrated.AcknowledgedAt == "" {
		t.Fatal("message migration lost ACK", err)
	}
}

func TestFutureRecordsAreNeverOverwritten(t *testing.T) {
	for _, kind := range []string{"session", "message", "adapter"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureStore(t)
			v, key := register(t, store, "cursor")
			path, _ := store.sessionPath(v.ID)
			msg := Message{ID: newID(), From: "codex:" + newID(), To: v.ID, Body: "ping", Status: StatusSubmitted}
			if kind == "message" {
				path = filepath.Join(store.Dir, "messages", msg.ID+".json")
				msg.Schema = 99
				if err := writeJSON(path, msg); err != nil {
					t.Fatal(err)
				}
			} else {
				if kind == "session" {
					v.Schema = 99
				} else {
					v.AdapterState = json.RawMessage(`{"schema":99}`)
				}
				if err := writeJSON(path, v); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			var err error
			if kind == "message" {
				_, err = store.Ack(v.ID, key, msg.ID)
			} else {
				err = store.Update(v.ID, StateBusy, false)
			}
			if ErrorCodeOf(err) != CodeUnsupportedSchema {
				t.Fatal("future schema was accepted", err)
			}
			if kind == "message" {
				_, err = store.Messages(v.ID, key)
			} else {
				_, err = store.List()
			}
			if ErrorCodeOf(err) != CodeUnsupportedSchema {
				t.Fatal("discovery skipped unsupported schema", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("future record changed")
			}
		})
	}
}
