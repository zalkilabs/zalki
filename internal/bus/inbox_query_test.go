package bus

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExactLookupAvoidsUnrelatedJournalReadsAndPreservesHistory(t *testing.T) {
	store := fixtureStore(t)
	sender, _ := register(t, store, "codex")
	receiver, key := register(t, store, "cursor")
	other, otherKey := register(t, store, "cursor")
	msg := Message{ID: newID(), From: sender.ID, To: receiver.ID, Body: "review result", CreatedAt: timestamp(), TargetGeneration: receiver.Generation, Status: StatusSubmitted}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	query := MessageQuery{Actor: receiver.ID, Capability: key, MessageID: msg.ID}
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", newID()+".json"), []byte("malformed unrelated record"), 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ReadMessages(query)
	if err != nil || len(batch.Messages) != 1 || batch.Messages[0].Body != msg.Body {
		t.Fatal("exact lookup scanned unrelated records or lost content", err)
	}
	query.Actor, query.Capability = other.ID, otherKey
	if _, err := store.ReadMessages(query); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("exact lookup exposed another session's message", err)
	}
	stored, err := store.message(msg.ID)
	if err != nil || stored.AcknowledgedAt != "" || stored.Body != msg.Body {
		t.Fatal("read changed durable content", err)
	}
}

func TestHistoryBodiesAreOptInAndNativeBodiesStayDeferred(t *testing.T) {
	store := fixtureStore(t)
	sender, key := register(t, store, "cursor")
	receiver, toKey := register(t, store, "codex")
	msg := Message{ID: newID(), From: sender.ID, To: receiver.ID, Body: "review result", CreatedAt: timestamp(), TargetGeneration: receiver.Generation, Status: StatusSubmitted}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	query := MessageQuery{Actor: sender.ID, Capability: key, History: true}
	batch, err := store.ReadMessages(query)
	if err != nil || len(batch.Messages) != 1 || batch.Messages[0].Body != "" || !batch.Messages[0].BodyOmitted {
		t.Fatal("history repeated bodies by default", err)
	}
	query.IncludeBody = true
	batch, err = store.ReadMessages(query)
	if err != nil || batch.Messages[0].Body != msg.Body {
		t.Fatal("explicit history body read lost content", err)
	}
	query = MessageQuery{Actor: receiver.ID, Capability: toKey, MessageID: msg.ID, IncludeBody: true}
	batch, err = store.ReadMessages(query)
	if err != nil || !batch.Messages[0].BodyDeferred || batch.Messages[0].Body != "" {
		t.Fatal("exact read bypassed native delivery", err)
	}
	stored, _ := store.message(msg.ID)
	if stored.BodyOmitted || stored.BodyDeferred || stored.Body != msg.Body {
		t.Fatal("response-only body flags reached the journal")
	}
}
