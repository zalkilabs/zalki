package bus

import (
	"context"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// A message can be reserved while its target is ready, and the target can then
// start waiting for a permission prompt before the message is delivered.
// Delivery must hold the message instead of sending it (revalidateTarget).

// reservedBeforeApproval reserves one message for a ready target, then moves
// the target to the approval state.
func reservedBeforeApproval(t *testing.T, store Store, body string) (model.Message, model.Session, model.Session, string) {
	t.Helper()
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	msg, _, fresh, err := store.ReserveMessage(SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: body})
	if err != nil || !fresh {
		t.Fatalf("reservation of %q: fresh=%v %v", body, fresh, err)
	}
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	return msg, sender, target, key
}

func requireHeldCountOf(t *testing.T, store Store, target string, want int) {
	t.Helper()
	if current, err := store.session(target); err != nil || current.ApprovalHeld != want {
		t.Fatalf("the target has %d held messages, want %d: %v", current.ApprovalHeld, want, err)
	}
}

func TestAMessageIsHeldWhenItsTargetEntersApprovalBeforeDelivery(t *testing.T) {
	store := fixtureStore(t)
	var delivered []string
	service := approvalService(t, store, &delivered)
	msg, _, target, _ := reservedBeforeApproval(t, store, "held")

	result, err := service.deliverReserved(context.Background(), msg, target)
	if err != nil || result.Status != model.StatusApprovalBlocked || !result.Held || result.Receipt == nil || result.Receipt.Status != model.StatusApprovalBlocked {
		t.Fatalf("a message for a target in approval was not held: status %q held %v %v", result.Status, result.Held, err)
	}
	stored, err := store.message(msg.ID)
	if err != nil || stored.Status != model.StatusApprovalBlocked || !stored.Held {
		t.Fatalf("the stored message was not held: status %q held %v %v", stored.Status, stored.Held, err)
	}
	requireHeldCountOf(t, store, target.ID, 1)
	if len(delivered) != 0 {
		t.Fatal("a message was sent to a target that waits for a prompt", delivered)
	}
	stopHook(t, service, "codex", target.ID)
	if len(delivered) != 1 || delivered[0] != "held" {
		t.Fatalf("the held message was not sent when the prompt ended: %v", delivered)
	}
}

func TestAFullApprovalQueueRefusesAMessageThatIsHeldAtDelivery(t *testing.T) {
	store := fixtureStore(t)
	var delivered []string
	service := approvalService(t, store, &delivered)
	msg, sender, target, key := reservedBeforeApproval(t, store, "late")
	for range approvalQueueCap {
		if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "waiting"}); err != nil {
			t.Fatal(err)
		}
	}

	result, err := service.deliverReserved(context.Background(), msg, target)
	if err != nil || result.Status != model.StatusNotSent || result.Receipt == nil || result.Receipt.Code != model.CodeUnavailable {
		t.Fatalf("a message that does not fit in the full queue must be refused: status %q %v", result.Status, err)
	}
	requireHeldCountOf(t, store, target.ID, approvalQueueCap)
	if len(delivered) != 0 {
		t.Fatal("a message was sent to a target that waits for a prompt", delivered)
	}
}
