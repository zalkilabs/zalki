package bus

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryTransitionsPreserveCompletedAndUncertainAttempts(t *testing.T) {
	for _, test := range []struct {
		from, to, want DeliveryStatus
		rejected       bool
	}{
		{StatusAttempting, StatusSubmitted, StatusSubmitted, false},
		{StatusAttempting, StatusUnknownTimeout, StatusUnknownTimeout, false},
		{StatusPendingActivity, StatusContextEmitted, StatusContextEmitted, false},
		{StatusContextEmitted, StatusWakeWritten, StatusContextEmitted, false},
		{StatusContextEmitted, StatusWakeUncertain, StatusContextEmitted, false},
		{StatusSubmitted, StatusAttempting, StatusSubmitted, true},
		{StatusUnknownTimeout, StatusSubmitted, StatusUnknownTimeout, true},
		{StatusApprovalBlocked, StatusSubmitted, StatusApprovalBlocked, true},
		{StatusSubmitted, DeliveryStatus("unknown"), StatusSubmitted, true},
	} {
		next, err := transition(test.from, test.to, false)
		if next != test.want || (err != nil) != test.rejected {
			t.Fatalf("%s -> %s = %s: %v", test.from, test.to, next, err)
		}
		if err != nil && ErrorCodeOf(fmt.Errorf("wrapped: %w", err)) != CodeInvalidTransition {
			t.Fatal("typed error code lost through wrapping")
		}
	}
	store := fixtureStore(t)
	from, key, to, _ := registerPair(t, store, "codex", "cursor")
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(msg.ID, Receipt{Status: StatusUnknownTimeout}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	before, _ := os.ReadFile(path)
	if _, err := store.Finish(msg.ID, Receipt{Status: StatusSubmitted}); ErrorCodeOf(err) != CodeInvalidTransition {
		t.Fatal("uncertain submission became a success", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid transition mutated journal")
	}
	if err := store.Update(to.ID, SessionState("invented"), false); ErrorCodeOf(err) != CodeInvalidTransition {
		t.Fatal("unknown session state accepted", err)
	}
}

func TestQueuedApprovalViewCannotBePersisted(t *testing.T) {
	if StatusQueuedForApproval.valid() || StatusQueuedForApproval.canAcknowledge() {
		t.Fatal("derived approval view was accepted as a persisted status")
	}
	if _, err := transition(StatusAttempting, StatusQueuedForApproval, false); ErrorCodeOf(err) != CodeInvalidTransition {
		t.Fatal("derived approval view became a journal transition", err)
	}
}

func TestHeldApprovalTransitionsDoNotReopenLegacyRefusals(t *testing.T) {
	for _, to := range []DeliveryStatus{StatusAttempting, StatusPendingActivity, StatusNotSent} {
		if _, err := transition(StatusApprovalBlocked, to, false); ErrorCodeOf(err) != CodeInvalidTransition {
			t.Fatalf("legacy refusal reopened to %s: %v", to, err)
		}
		if got, err := transition(StatusApprovalBlocked, to, true); err != nil || got != to {
			t.Fatalf("held transition to %s rejected: %v", to, err)
		}
	}
	legacy := Message{Status: StatusApprovalBlocked}
	if err := legacy.releaseHeld(StatusAttempting); ErrorCodeOf(err) != CodeInvalidTransition || legacy.Held || legacy.Status != StatusApprovalBlocked {
		t.Fatal("release helper changed a legacy refusal", err)
	}
	msg := Message{Status: StatusAttempting}
	if err := msg.hold(); err != nil || !msg.IsQueuedForApproval() || msg.Receipt.Error != approvalQueuedNote {
		t.Fatal("hold did not preserve canonical journal state", err)
	}
	if err := msg.releaseHeld(StatusPendingActivity); err != nil || msg.Held || msg.Receipt.Status != StatusPendingActivity {
		t.Fatal("hook release lost its receipt", err)
	}
	if err := msg.emitContext(); err != nil {
		t.Fatal(err)
	}
	if err := msg.applyReceipt(Receipt{Status: StatusWakeWritten}); err != nil || msg.Status != StatusContextEmitted {
		t.Fatal("racing wake overwrote emitted context", err)
	}
}
