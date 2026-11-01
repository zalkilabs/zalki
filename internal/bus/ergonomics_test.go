package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSendGeneratesIDsAndExplicitRetryNeverResubmits(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "cursor")
	to, _ := register(t, store, "codex")
	calls := 0
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		calls++
		return NewReceipt(StatusSubmitted)
	}}
	request := SendRequest{Actor: from.ID, Capability: key, To: to.ID, Body: "intentional repeat"}
	first, err := service.Send(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UUID(first.ID); err != nil {
		t.Fatal("missing generated UUID", err)
	}
	request.MessageID = first.ID
	again, err := service.Send(context.Background(), request)
	if err != nil || again.ID != first.ID || calls != 1 {
		t.Fatal("explicit retry resubmitted", err)
	}
	request.MessageID = ""
	second, err := service.Send(context.Background(), request)
	if err != nil || second.ID == first.ID || calls != 2 {
		t.Fatal("identical intentional send was deduplicated", err)
	}
	request.MessageID = "invalid"
	if _, err := service.Send(context.Background(), request); ErrorCodeOf(err) != CodeInvalidIdentity || calls != 2 {
		t.Fatal("explicit invalid ID was replaced or submitted", err)
	}
}

func TestBusyDeliveryObservationIsNotPersisted(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "cursor")
	to, _ := register(t, store, "codex")
	if err := store.Update(to.ID, StateBusy, false); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt { return NewReceipt(StatusSubmitted) }}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, Body: "review"})
	if err != nil || msg.BusyDelivery != DeliveryNextTurn {
		t.Fatal("delivery lost busy target timing", err)
	}
	stored, err := store.message(msg.ID)
	if err != nil || stored.BusyDelivery != "" {
		t.Fatal("historical message retained a current-state observation", err)
	}
	encoded, _ := json.Marshal(msg)
	if strings.Contains(string(encoded), "BusyDelivery") || strings.Contains(string(encoded), "busy_delivery") {
		t.Fatal("delivery observation leaked into CLI/journal JSON")
	}
}

func TestSentHistoryFiltersPaginatesAndKeepsReceipts(t *testing.T) {
	store := fixtureStore(t)
	actor, key := register(t, store, "cursor")
	peer, _ := register(t, store, "codex")
	other, otherKey := register(t, store, "cursor")
	want := map[string]Message{}
	var incoming string
	for i := range 12 {
		msg := Message{ID: newID(), From: actor.ID, To: peer.ID, Body: "history", CreatedAt: fmt.Sprintf("2026-11-01T00:00:%02d.000000000Z", i), Status: StatusSubmitted, AcknowledgedAt: timestamp()}
		if i%2 == 0 {
			msg.From, msg.To = peer.ID, actor.ID
			msg.TargetGeneration = actor.Generation
			incoming = msg.ID
		} else {
			if i == 3 || i == 5 || i == 7 {
				msg.Status = map[int]DeliveryStatus{3: StatusPendingActivity, 5: StatusApprovalBlocked, 7: StatusNotSent}[i]
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
	if _, err := store.ReadMessages(query); ErrorCodeOf(err) != CodeNotFound {
		t.Fatal("sent-only exact lookup returned an incoming message", err)
	}
	for id := range want {
		query.Actor, query.Capability, query.MessageID = other.ID, otherKey, id
		if _, err := store.ReadMessages(query); ErrorCodeOf(err) != CodeUnauthorized {
			t.Fatal("another session could read sent receipts", err)
		}
		break
	}
	query = MessageQuery{Actor: actor.ID, Capability: key, Sent: true}
	query.Capability = "invalid"
	if _, err := store.ReadMessages(query); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("sent listing bypassed authentication", err)
	}
}
