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

	"github.com/zalkilabs/zalki/internal/model"
)

// nearLimitMessage pads msg's body so its record is 4 bytes under the encoded
// limit, writes that record, and returns its path and bytes. Any rewrite that
// grows the record (a longer status, a receipt, an acknowledgement) is then
// over the limit.
func nearLimitMessage(t *testing.T, store Store, msg *model.Message) (string, []byte) {
	t.Helper()
	if err := store.locked(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	msg.Schema = recordSchema
	base, err := encodeStateJSON(*msg)
	if err != nil {
		t.Fatal(err)
	}
	msg.Body += strings.Repeat("x", maxStateFileBytes-4-len(base))
	raw, err := encodeStateJSON(*msg)
	if err != nil || len(raw) != maxStateFileBytes-4 {
		t.Fatalf("fixture is %d bytes, want %d: %v", len(raw), maxStateFileBytes-4, err)
	}
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.message(msg.ID); err != nil {
		t.Fatal("near-limit input was unreadable", err)
	}
	return path, raw
}

func requireEncodedSizeError(t *testing.T, err error) {
	t.Helper()
	if model.ErrorCodeOf(err) != model.CodeInvalidInput || !strings.Contains(err.Error(), "encoded state record") || !strings.Contains(err.Error(), strconv.Itoa(maxStateFileBytes)) {
		t.Fatal("missing clear encoded-size refusal", err)
	}
}

// A rewrite that grows over the encoded limit is refused, and the original
// record stays as it was and readable.
func TestARewriteOverTheLimitLeavesTheOriginal(t *testing.T) {
	store := fixtureStore(t)
	msg := model.Message{ID: newID(), From: "codex:" + newID(), To: "cursor:" + newID(), Body: "fixture", Status: model.StatusSubmitted}
	path, before := nearLimitMessage(t, store, &msg)
	msg.Status = model.StatusPendingActivity
	requireEncodedSizeError(t, store.saveMessage(msg))
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected rewrite changed original record", err)
	}
	if _, err := store.message(msg.ID); err != nil {
		t.Fatal("rejected rewrite made original unreadable", err)
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
			status := model.StatusSubmitted
			if operation == "receipt" {
				status = model.StatusAttempting
			}
			msg := model.Message{ID: newID(), From: from.ID, To: target.ID, TargetGeneration: target.Generation, Body: "private-body-size-sentinel", Status: status}
			path, before := nearLimitMessage(t, store, &msg)
			snapshot := journalSnapshot(t, store)
			var diagnostic bytes.Buffer
			store.Logger = slog.New(slog.NewJSONHandler(&diagnostic, nil))
			var result model.Message
			var err error
			wantEvent := "message.ack_failed"
			if operation == "receipt" {
				result, err = store.Finish(msg.ID, model.NewReceipt(model.StatusSubmitted))
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
			if len(rows) != 1 || rows[0]["msg"] != wantEvent || rows[0]["message_id"] != msg.ID || rows[0]["error_code"] != string(model.CodeInvalidInput) {
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
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		calls++
		return model.NewReceipt(model.StatusSubmitted)
	}
	body := strings.Repeat("\x01", 16*1024)
	msg, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: target.ID, Body: body})
	if err != nil || msg.Status != model.StatusSubmitted || calls != 1 {
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
