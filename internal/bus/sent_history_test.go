package bus

import (
	"fmt"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestSentHistoryFiltersPaginatesAndKeepsReceipts(t *testing.T) {
	store := fixtureStore(t)
	actor, key, peer, _ := registerPair(t, store, "cursor", "codex")
	other, otherKey := register(t, store, "cursor")
	want := map[string]Message{}
	var incoming string
	for i := range 12 {
		msg := Message{ID: newID(), From: actor.ID, To: peer.ID, Body: "history", CreatedAt: fmt.Sprintf("2026-11-01T00:00:%02d.000000000Z", i), Status: model.StatusSubmitted, AcknowledgedAt: model.Timestamp()}
		if i%2 == 0 {
			msg.From, msg.To = peer.ID, actor.ID
			msg.TargetGeneration = actor.Generation
			incoming = msg.ID
		} else {
			if i == 3 || i == 5 || i == 7 {
				msg.Status = map[int]model.DeliveryStatus{3: model.StatusPendingActivity, 5: model.StatusApprovalBlocked, 7: model.StatusNotSent}[i]
				msg.AcknowledgedAt = ""
			}
			want[msg.ID] = msg
		}
		if err := store.saveMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	query := MessageQuery{Actor: actor.ID, Capability: key, Sent: true, Limit: 2}
	seen := map[string]bool{}
	previous := "9999"
	for {
		page, err := store.ReadMessages(query)
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range page.Messages {
			original, ok := want[msg.ID]
			if !ok || seen[msg.ID] || msg.From != actor.ID || msg.Body != "" || !msg.BodyOmitted || msg.AcknowledgedAt != original.AcknowledgedAt || msg.Status != original.Status || msg.CreatedAt > previous {
				t.Fatalf("sent page lost direction, summary, receipt, or order: %+v", msg)
			}
			seen[msg.ID], previous = true, msg.CreatedAt
		}
		if !page.More {
			break
		}
		query.Cursor = page.Messages[len(page.Messages)-1].ID
	}
	if len(seen) != len(want) {
		t.Fatal("sent pagination lost records")
	}
	query.Cursor, query.IncludeBody = "", true
	page, err := store.ReadMessages(query)
	if err != nil || len(page.Messages) != 2 || page.Messages[0].Body != "history" {
		t.Fatal("sent bodies were not opt-in", err)
	}
	query.MessageID = incoming
	if _, err := store.ReadMessages(query); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("sent-only exact lookup returned an incoming message", err)
	}
	for id := range want {
		query.Actor, query.Capability, query.MessageID = other.ID, otherKey, id
		if _, err := store.ReadMessages(query); model.ErrorCodeOf(err) != model.CodeUnauthorized {
			t.Fatal("another session could read sent receipts", err)
		}
		break
	}
	query = MessageQuery{Actor: actor.ID, Capability: key, Sent: true}
	query.Capability = "invalid"
	if _, err := store.ReadMessages(query); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("sent listing bypassed authentication", err)
	}
}
