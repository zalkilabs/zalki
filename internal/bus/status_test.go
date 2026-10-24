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
		next, err := transition(test.from, test.to)
		if next != test.want || (err != nil) != test.rejected {
			t.Fatalf("%s -> %s = %s: %v", test.from, test.to, next, err)
		}
		if err != nil && ErrorCodeOf(fmt.Errorf("wrapped: %w", err)) != CodeInvalidTransition {
			t.Fatal("typed error code lost through wrapping")
		}
	}
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, _ := register(t, store, "cursor")
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
