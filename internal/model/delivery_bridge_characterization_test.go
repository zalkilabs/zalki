package model

import (
	"fmt"
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
		next, err := Transition(test.from, test.to, false)
		if next != test.want || (err != nil) != test.rejected {
			t.Fatalf("%s -> %s = %s: %v", test.from, test.to, next, err)
		}
		if err != nil && ErrorCodeOf(fmt.Errorf("wrapped: %w", err)) != CodeInvalidTransition {
			t.Fatal("typed error code lost through wrapping")
		}
	}
}

func TestQueuedApprovalViewCannotBePersisted(t *testing.T) {
	if StatusQueuedForApproval.Valid() || StatusQueuedForApproval.CanAcknowledge() {
		t.Fatal("derived approval view was accepted as a persisted status")
	}
	if _, err := Transition(StatusAttempting, StatusQueuedForApproval, false); ErrorCodeOf(err) != CodeInvalidTransition {
		t.Fatal("derived approval view became a journal transition", err)
	}
}

func TestHeldApprovalTransitionsDoNotReopenLegacyRefusals(t *testing.T) {
	for _, to := range []DeliveryStatus{StatusAttempting, StatusPendingActivity, StatusNotSent} {
		if _, err := Transition(StatusApprovalBlocked, to, false); ErrorCodeOf(err) != CodeInvalidTransition {
			t.Fatalf("legacy refusal reopened to %s: %v", to, err)
		}
		if got, err := Transition(StatusApprovalBlocked, to, true); err != nil || got != to {
			t.Fatalf("held transition to %s rejected: %v", to, err)
		}
	}
	legacy := Message{Status: StatusApprovalBlocked}
	if err := legacy.ReleaseHeld(StatusAttempting); ErrorCodeOf(err) != CodeInvalidTransition || legacy.Held || legacy.Status != StatusApprovalBlocked {
		t.Fatal("release helper changed a legacy refusal", err)
	}
	msg := Message{Status: StatusAttempting}
	if err := msg.Hold(); err != nil || !msg.IsQueuedForApproval() || msg.Receipt.Error != ApprovalQueuedNote {
		t.Fatal("hold did not preserve canonical journal state", err)
	}
	if err := msg.ReleaseHeld(StatusPendingActivity); err != nil || msg.Held || msg.Receipt.Status != StatusPendingActivity {
		t.Fatal("hook release lost its receipt", err)
	}
	if err := msg.EmitContext(); err != nil {
		t.Fatal(err)
	}
	if err := msg.ApplyReceipt(Receipt{Status: StatusWakeWritten}); err != nil || msg.Status != StatusContextEmitted {
		t.Fatal("racing wake overwrote emitted context", err)
	}
}
