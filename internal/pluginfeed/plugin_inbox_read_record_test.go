package pluginfeed

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxReadIndexMustRemainActiveAndValid(t *testing.T) {
	for _, kind := range []string{"missing", "null", "empty-object", "missing-version", "missing-installation", "missing-quota", "empty-quota", "missing-entries", "null-entries", "expired", "future-entry", "orphan"} {
		t.Run(kind, func(t *testing.T) { checkInboxReadIndex(t, kind) })
	}
}
func checkInboxReadIndex(t *testing.T, kind string) {
	t.Helper()
	x := newInboxReadFixture(t, "")
	path := filepath.Join(x.f.pluginDataPath(), "inbox.json")
	now := x.now
	switch kind {
	case "missing":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	case "orphan":
		if err := os.Remove(filepath.Join(x.f.store.Dir, "messages", x.message.ID+".json")); err != nil {
			t.Fatal(err)
		}
	case "expired":
		now = x.index(t).Entries[0].At.Add(31 * 24 * time.Hour)
	case "future-entry":
		index := x.index(t)
		index.Entries[0].At = now.Add(time.Hour)
		x.putIndex(t, index)
	default:
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "null":
			raw = []byte(`null`)
		case "empty-object":
			raw = []byte(`{}`)
		default:
			poisonExistingIndexField(fields, kind)
			raw, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := journalSnapshot(t, x.f.store)
	raw, err := x.read(t, &x.status, now)
	assertInboxReadDenied(t, raw, err, "")
	after := journalSnapshot(t, x.f.store)
	delete(after, "plugin-inbox-read-audit.json")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read repaired/pruned index, quota or canonical state")
	}
}
func TestPluginInboxReadCanonicalRecordMustMatchReservation(t *testing.T) {
	for _, kind := range []string{"null", "empty-object", "unknown-field", "wrong-schema", "wrong-id", "wrong-to", "wrong-from", "wrong-generation", "wrong-created-at", "coherent-created-at", "wrong-size", "body-size", "held", "acknowledged", "pending", "notice", "reply", "body-omitted", "body-deferred", "receipt-missing", "receipt-status", "receipt-time", "receipt-code", "receipt-error", "receipt-output", "empty-body", "nul-body", "over-16KiB"} {
		t.Run(kind, func(t *testing.T) { checkInboxReadRecord(t, kind) })
	}
}
func checkInboxReadRecord(t *testing.T, kind string) {
	t.Helper()
	x := newInboxReadFixture(t, "")
	m := x.message
	editInboxReadRecord(&m, kind)
	raw, err := journal.EncodeJSON(m, maxStateFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "null":
		raw = []byte(`null`)
	case "empty-object":
		raw = []byte(`{}`)
	case "unknown-field":
		raw = bytes.Replace(raw, []byte(`"schema":`), []byte(`"secret_unknown":true,"schema":`), 1)
	}
	if err = os.WriteFile(filepath.Join(x.f.store.Dir, "messages", x.message.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Match the encoded length so every other fence is tested independently.
	index := x.index(t)
	if kind != "body-size" {
		index.Entries[0].Bytes = len(raw)
	}
	if kind == "wrong-size" {
		index.Entries[0].Bytes++
	}
	x.putIndex(t, index)
	before := journalSnapshot(t, x.f.store)
	out, err := x.read(t, &x.status, x.now)
	assertInboxReadDenied(t, out, err, "")
	log := x.audit(t)
	outcome := "denied"
	if kind == "empty-body" || kind == "nul-body" || kind == "over-16KiB" {
		outcome = "too_large"
	}
	if len(log.Rows) != 1 || log.Rows[0].Outcome != outcome || log.Rows[0].Bytes != 0 {
		t.Fatal("canonical refusal lacked matching zero-byte audit", kind)
	}
	after := journalSnapshot(t, x.f.store)
	delete(after, "plugin-inbox-read-audit.json")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("canonical refusal changed stored state")
	}
}
func editInboxReadRecord(m *model.Message, kind string) {
	switch kind {
	case "wrong-schema":
		m.Schema = 99
	case "wrong-id":
		m.ID = model.NewUUID()
	case "wrong-to":
		m.To = "plugin:" + model.NewUUID()
	case "wrong-from":
		m.From = "cursor:" + model.NewUUID()
	case "wrong-generation":
		m.TargetGeneration = model.NewUUID()
	case "wrong-created-at":
		m.CreatedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	case "held":
		m.Held = true
	case "acknowledged":
		m.AcknowledgedAt = m.CreatedAt
	case "pending":
		m.Status = model.StatusPendingActivity
	case "notice":
		m.Kind = model.NoticeKind
	case "reply":
		m.ReplyTo = model.NewUUID()
	case "body-omitted":
		m.BodyOmitted = true
	case "body-deferred":
		m.BodyDeferred = true
	default:
		editInboxReadReceiptAndBody(m, kind)
	}
}
func editInboxReadReceiptAndBody(m *model.Message, kind string) {
	switch kind {
	case "coherent-created-at":
		m.CreatedAt = "2000-01-01T00:00:00Z"
		m.Receipt.At = m.CreatedAt
	case "body-size":
		m.Body += " modified"
	case "receipt-missing":
		m.Receipt = nil
	case "receipt-status":
		m.Receipt.Status = model.StatusPendingActivity
	case "receipt-time":
		m.Receipt.At = "2000-01-01T00:00:00Z"
	case "receipt-code":
		m.Receipt.Code = model.CodeUnavailable
	case "receipt-error":
		m.Receipt.Error = "PRIVATE-RECEIPT-ERROR"
	case "receipt-output":
		m.Receipt.Output = "PRIVATE-RECEIPT-OUTPUT"
	case "empty-body":
		m.Body = ""
	case "nul-body":
		m.Body = "private\x00body"
	case "over-16KiB":
		m.Body = strings.Repeat("a", (16<<10)+1)
	}
}
