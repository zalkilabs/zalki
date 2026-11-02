package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// A released or future build may write compact JSON that is readable but grows
// beyond the journal bound when an extension is indented during a rewrite.
func compactExtendedMessage(t *testing.T, store Store, msg Message) (string, []byte) {
	t.Helper()
	if err := store.locked(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	msg.Schema = recordSchema
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	extension, err := json.Marshal(make([]int, 20000))
	if err != nil {
		t.Fatal(err)
	}
	fields["future_extension"] = extension
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if len(raw) >= maxStateFileBytes {
		t.Fatal("fixture is not initially below the read limit")
	}
	if _, err := store.message(msg.ID); err != nil {
		t.Fatal("compact input was unreadable", err)
	}
	return path, raw
}

func requireEncodedSizeError(t *testing.T, err error) {
	t.Helper()
	if ErrorCodeOf(err) != CodeInvalidInput || !strings.Contains(err.Error(), "encoded state record") || !strings.Contains(err.Error(), strconv.Itoa(maxStateFileBytes)) {
		t.Fatal("missing clear encoded-size refusal", err)
	}
}

func TestPreservedExtensionsRemainWithinEncodedReadLimit(t *testing.T) {
	store := fixtureStore(t)
	msg := Message{ID: newID(), From: "codex:" + newID(), To: "cursor:" + newID(), Body: "fixture", Status: StatusSubmitted}
	path, before := compactExtendedMessage(t, store, msg)
	requireEncodedSizeError(t, store.saveMessage(msg))
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected merge changed original record", err)
	}
	if _, err := store.message(msg.ID); err != nil {
		t.Fatal("rejected rewrite made original unreadable", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, stateFormatFile)); !os.IsNotExist(err) {
		t.Fatal("oversized record published a marker before validation", err)
	}
}

func TestStateJSONExactEncodedLimitAndOneByteOverflow(t *testing.T) {
	type record struct {
		Payload string `json:"payload"`
	}
	empty, err := json.MarshalIndent(record{}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	exact := record{Payload: strings.Repeat("x", maxStateFileBytes-len(empty)-1)}
	path := filepath.Join(t.TempDir(), "record.json")
	if err := writeJSON(path, exact); err != nil {
		t.Fatal("exact-limit record was refused", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != maxStateFileBytes || before[len(before)-1] != '\n' {
		t.Fatal("test did not hit exact persisted limit", len(before))
	}
	var loaded record
	if err := readJSON(path, &loaded); err != nil || loaded != exact {
		t.Fatal("writer accepted a record reader refused", err)
	}
	overflow := record{Payload: exact.Payload + "x"}
	requireEncodedSizeError(t, writeJSON(path, overflow))
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("overflow replaced original record", err)
	}
	latest, err := os.Stat(path)
	if err != nil || !os.SameFile(info, latest) {
		t.Fatal("overflow renamed a new file over original", err)
	}
	absent := filepath.Join(filepath.Dir(path), "absent.json")
	requireEncodedSizeError(t, writeJSON(absent, overflow))
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("overflow created a new state file", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatal("overflow left temporary files", err)
	}
}

func TestMessageWriteSizeRefusalKeepsDurableStatusAndLogsFailure(t *testing.T) {
	for _, operation := range []string{"receipt", "ack"} {
		t.Run(operation, func(t *testing.T) {
			store := fixtureStore(t)
			from, _, target, key := registerPair(t, store, "codex", "cursor")
			status := StatusSubmitted
			if operation == "receipt" {
				status = StatusAttempting
			}
			msg := Message{ID: newID(), From: from.ID, To: target.ID, TargetGeneration: target.Generation, Body: "private-body-size-sentinel", Status: status}
			path, before := compactExtendedMessage(t, store, msg)
			snapshot := journalSnapshot(t, store)
			var diagnostic bytes.Buffer
			store.Logger = slog.New(slog.NewJSONHandler(&diagnostic, nil))
			var result Message
			var err error
			wantEvent := "message.ack_failed"
			if operation == "receipt" {
				result, err = store.Finish(msg.ID, NewReceipt(StatusSubmitted))
				wantEvent = "message.receipt_save_failed"
			} else {
				result, err = store.Ack(target.ID, key, msg.ID)
			}
			requireEncodedSizeError(t, err)
			if result.Status != status || result.Receipt != nil || result.AcknowledgedAt != "" {
				t.Fatal("failure returned uncommitted receipt or status", result.Status)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failure changed durable record", err)
			}
			durable, err := store.message(msg.ID)
			if err != nil || durable.Status != status || durable.AcknowledgedAt != "" {
				t.Fatal("previous status did not remain readable", err)
			}
			if after := journalSnapshot(t, store); !reflect.DeepEqual(snapshot, after) {
				t.Fatal("size refusal changed the journal or marker")
			}
			rows := logRows(t, &diagnostic)
			if len(rows) != 1 || rows[0]["msg"] != wantEvent || rows[0]["message_id"] != msg.ID || rows[0]["error_code"] != string(CodeInvalidInput) {
				t.Fatal("write refusal was not logged", rows)
			}
			if strings.Contains(diagnostic.String(), msg.Body) || strings.Contains(diagnostic.String(), key) {
				t.Fatal("write refusal leaked private content")
			}
		})
	}
}

func TestMaxControlCharacterBodyFitsEncodedJournalLimit(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	from, key, target, targetKey := registerPair(t, store, "cursor", "codex")
	calls := 0
	service.Deliver = func(context.Context, Message, Session) Receipt { calls++; return NewReceipt(StatusSubmitted) }
	body := strings.Repeat("\x01", 16*1024)
	msg, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: target.ID, Body: body})
	if err != nil || msg.Status != StatusSubmitted || calls != 1 {
		t.Fatal("normal maximum body was refused", err)
	}
	if _, err := store.Ack(target.ID, targetKey, msg.ID); err != nil {
		t.Fatal("maximum-body ACK failed", err)
	}
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxStateFileBytes || !bytes.Contains(raw, []byte(`\u0001`)) {
		t.Fatal("control body encoding was not bounded", len(raw))
	}
	loaded, err := store.message(msg.ID)
	if err != nil || loaded.Body != body || loaded.AcknowledgedAt == "" {
		t.Fatal("maximum body did not round-trip", err)
	}
}
