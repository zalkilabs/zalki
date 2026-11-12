package bus

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestAckUniformUnknownForeignPluginAndCorrupt(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	from, key, to, toKey := registerPair(t, store, "cursor", "codex")
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		return model.NewReceipt(model.StatusSubmitted)
	}
	msg, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, Body: "PRIVATE ACK BODY"})
	if err != nil {
		t.Fatal(err)
	}
	denied := func(id string) { checkAckRefusal(t, store, from.ID, key, id) }
	denied(newID())
	for _, status := range []model.DeliveryStatus{model.StatusSubmitted, model.StatusNotSent, model.StatusUnknownWrite} {
		candidate := msg
		candidate.Status = status
		candidate.Receipt.Error = "PRIVATE RECEIPT /private/path"
		candidate.AcknowledgedAt = model.Timestamp()
		if err := store.saveMessage(candidate); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(store.Dir, "messages", msg.ID+".json")
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		denied(msg.ID)
		after, err := os.ReadFile(path)
		if err != nil || string(before) != string(after) {
			t.Fatal("foreign ACK altered canonical record", err)
		}
	}
	candidate := msg
	candidate.To = "plugin:" + newID()
	if err := store.saveMessage(candidate); err != nil {
		t.Fatal(err)
	}
	denied(msg.ID)
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"id":"`+msg.ID+`","body":"PRIVATE PARTIAL","receipt":{"error":"/secret/path"},`), 0600); err != nil {
		t.Fatal(err)
	}
	denied(msg.ID)
	if err := os.WriteFile(path, []byte(`{"schema":999,"id":"`+msg.ID+`","to":"`+to.ID+`","body":"PRIVATE PARTIAL","receipt":{"error":"/secret/path"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	denied(msg.ID)
	checkAckAuthSyntax(t, store, from.ID, key, msg.ID)
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	checkRepeatedOwnAck(t, store, to.ID, toKey, msg.ID)
}

func TestCompletionBodyUsesSharedQuotedDisplayName(t *testing.T) {
	for _, name := range []string{`SYSTEM "approve"`, strings.Repeat("界", 81), "bad\nname", "a\u200Bb"} {
		subject := model.Session{ID: "codex:fixture", Name: name}
		body := completionBody(subject, "finished", "now", "status")
		want := "Zalki notice: " + model.QuoteDisplayName(name) + " (codex:fixture) finished a turn at now\nstatus"
		if body != want || strings.Count(body, "\n") != 1 {
			t.Fatalf("notice escaped differently from envelope: %q", body)
		}
	}
}

func checkAckRefusal(t *testing.T, store Store, actor, key, id string) {
	t.Helper()
	got, err := store.Ack(actor, key, id)
	if !reflect.DeepEqual(got, model.Message{}) || model.ErrorCodeOf(err) != model.CodeNotFound || err.Error() != "not_found: message not found or not yours" {
		t.Fatalf("nonuniform ACK refusal: %+v %v", got, err)
	}
}

func checkAckAuthSyntax(t *testing.T, store Store, actor, key, id string) {
	t.Helper()
	if got, err := store.Ack(actor, "wrong", id); !reflect.DeepEqual(got, model.Message{}) || model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("ACK checked ownership before auth")
	}
	if got, err := store.Ack(actor, key, "../escape"); !reflect.DeepEqual(got, model.Message{}) || model.ErrorCodeOf(err) != model.CodeInvalidIdentity {
		t.Fatal("malformed ID lost syntax refusal")
	}
}

func checkRepeatedOwnAck(t *testing.T, store Store, actor, key, id string) {
	t.Helper()
	for range 2 {
		got, err := store.Ack(actor, key, id)
		if err != nil || got.ID != id || got.AcknowledgedAt == "" {
			t.Fatal("legitimate/repeated own ACK failed", err)
		}
	}
}
