package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// Records are written as plain typed values: a field the value omits is gone
// after the write, and so is a field this build does not know.
func TestARewriteKeepsNoFieldTheValueOmits(t *testing.T) {
	store := fixtureStore(t)
	from, _, to, _ := registerPair(t, store, "codex", "cursor")
	msg := model.Message{Schema: recordSchema, ID: newID(), From: from.ID, To: to.ID, Body: "body",
		TargetGeneration: to.Generation, CreatedAt: model.Timestamp(), Status: model.StatusSubmitted, AcknowledgedAt: model.Timestamp()}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	var record map[string]any
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	record["unknown_field"] = "kept by nobody"
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
	msg.AcknowledgedAt = ""
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := readJSON(path, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"acknowledged_at", "unknown_field"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("the rewrite kept %q: %v", key, fields)
		}
	}
}

// Reply recovery writes the typed reply from the intent, with every field.
func TestReplyRecoveryWritesTheTypedReply(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "codex")
	sender, _ := register(t, store, "codex")
	parent := model.Message{Schema: recordSchema, ID: newID(), From: target.ID, To: sender.ID, Body: "question",
		TargetGeneration: sender.Generation, Status: model.StatusSubmitted, CreatedAt: model.Timestamp()}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	reply := model.Message{Schema: recordSchema, ID: replyID(sender.ID, parent.ID, "answer"), From: sender.ID,
		FromName: "sender name", FromLabel: "Codex", To: target.ID, Body: "answer", ReplyTo: parent.ID,
		TargetGeneration: target.Generation, Status: model.StatusAttempting, CreatedAt: model.Timestamp()}
	if err := privateDir(filepath.Join(store.Dir, "transactions")); err != nil {
		t.Fatal(err)
	}
	tx := replyTransaction{Schema: 1, ParentID: parent.ID, AcknowledgedAt: model.Timestamp(), Reply: reply}
	if err := store.writeRecord(filepath.Join(store.Dir, "transactions", reply.ID+".json"), tx); err != nil {
		t.Fatal(err)
	}
	if err := store.locked(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := store.message(reply.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != reply.Body || got.FromName != reply.FromName || got.FromLabel != reply.FromLabel || got.ReplyTo != parent.ID || got.CreatedAt != reply.CreatedAt {
		t.Fatalf("recovered reply = %+v, want the intent's reply %+v", got, reply)
	}
}

// The size check and the writer use one encoder, so the measured size is the
// size of the file that is written.
func TestEncodedMessageSizeIsTheWrittenSize(t *testing.T) {
	store := fixtureStore(t)
	from, _, to, _ := registerPair(t, store, "codex", "cursor")
	msg := model.Message{Schema: recordSchema, ID: newID(), From: from.ID, To: to.ID, Body: "measure me",
		TargetGeneration: to.Generation, CreatedAt: model.Timestamp(), Status: model.StatusSubmitted}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.Dir, "messages", msg.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	size, err := EncodedMessageSize(msg)
	if err != nil || int64(size) != info.Size() {
		t.Fatalf("EncodedMessageSize = %d, %v; file is %d bytes", size, err, info.Size())
	}
}

// A normal writer never overwrites a record it cannot trust: a previous record
// with a newer schema, or one that cannot be read, is refused and its bytes are
// unchanged.
func TestAWriterNeverOverwritesAnUntrustedRecord(t *testing.T) {
	for name, content := range map[string]string{
		"newer schema": `{"schema":2,"id":"ID","body":"from another build"}`,
		"not json":     `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			store := fixtureStore(t)
			from, _, to, _ := registerPair(t, store, "codex", "cursor")
			msg := model.Message{Schema: recordSchema, ID: newID(), From: from.ID, To: to.ID, Body: "new",
				TargetGeneration: to.Generation, CreatedAt: model.Timestamp(), Status: model.StatusSubmitted}
			path := filepath.Join(store.Dir, "messages", msg.ID+".json")
			before := []byte(strings.ReplaceAll(content, "ID", msg.ID))
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			if err := store.saveMessage(msg); err == nil {
				t.Fatal("the writer overwrote an untrusted record")
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("the untrusted record changed: %s %v", after, err)
			}
		})
	}
}

// One record format: a rewrite over an earlier record is byte-identical to a
// first write of the same value (struct field order, nothing kept from before).
func TestARewriteIsByteIdenticalToAFirstWrite(t *testing.T) {
	store := fixtureStore(t)
	from, _, to, _ := registerPair(t, store, "codex", "cursor")
	msg := model.Message{Schema: recordSchema, ID: newID(), From: from.ID, FromLabel: "Codex", To: to.ID, Body: "final",
		TargetGeneration: to.Generation, CreatedAt: model.Timestamp(), Status: model.StatusSubmitted,
		Receipt: &model.Receipt{At: model.Timestamp(), Status: model.StatusSubmitted}}
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	earlier := msg
	earlier.Body, earlier.Status, earlier.Receipt = "earlier", model.StatusAttempting, nil
	if err := store.saveMessage(earlier); err != nil {
		t.Fatal(err)
	}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(first, rewritten) {
		t.Fatalf("rewrite differs from a first write:\n%s\n---\n%s", rewritten, first)
	}
}
