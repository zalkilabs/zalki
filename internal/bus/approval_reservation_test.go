package bus

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHeldReservationWriteFailureRollsBackTheCount(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "cursor")
	msg := Message{ID: newID(), To: target.ID, Status: StatusAttempting}
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	// A directory at the final filename forces atomic rename to fail, even as root.
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.locked(func() error { return store.saveReservation(msg) }); err == nil {
		t.Fatal("reservation unexpectedly saved over a directory")
	}
	current, err := store.session(target.ID)
	if err != nil || current.ApprovalHeld != 0 {
		t.Fatal("failed reservation inflated held count", err)
	}
}

func TestHookPollPreservesApprovalCountUntilResolution(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "cursor")
	target.State, target.ApprovalHeld = StateApproval, 1
	if err := store.saveSession(target); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EmitPending(target.ID); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(target.ID)
	if err != nil || current.ApprovalHeld != 1 {
		t.Fatal("hook poll changed still-open approval count", err)
	}
	if err := (&Service{Store: store}).releaseApprovalQueue(t.Context(), target.ID); err != nil {
		t.Fatal(err)
	}
	current, err = store.session(target.ID)
	if err != nil || current.ApprovalHeld != 0 {
		t.Fatal("lifecycle release failed to repair an empty queue", err)
	}
}
