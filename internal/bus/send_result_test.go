package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestSendGeneratesIDsAndExplicitRetryNeverResubmits(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	from, key, to, _ := registerPair(t, store, "cursor", "codex")
	calls := 0
	service.Deliver = func(context.Context, Message, Session) Receipt {
		calls++
		return NewReceipt(StatusSubmitted)
	}
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
	service := newTestService(t)
	store := service.Store
	from, key, to, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(to.ID, StateBusy, false); err != nil {
		t.Fatal(err)
	}
	service.Deliver = func(context.Context, Message, Session) Receipt { return NewReceipt(StatusSubmitted) }
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, Body: "review"})
	if err != nil || msg.BusyDelivery != model.DeliveryNextTurn {
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
