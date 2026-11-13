package bus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestHeldReservationUnreadableFailureKeepsCountUntilRecount(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "cursor")
	msg := model.Message{ID: newID(), To: target.ID, Status: model.StatusAttempting}
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	// A directory at the final filename refuses the write and makes the hold
	// inventory unreadable. Error alone no longer proves that decrement is safe.
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.locked(func() error { return store.saveReservation(msg) }); err == nil {
		t.Fatal("reservation unexpectedly saved over a directory")
	}
	current, err := store.session(target.ID)
	if err != nil || current.ApprovalHeld != 1 {
		t.Fatal("unreadable reservation inventory lost its conservative count", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := (&Service{Store: store}).releaseApprovalQueue(t.Context(), target.ID); err != nil {
		t.Fatal(err)
	}
	current, err = store.session(target.ID)
	if err != nil || current.ApprovalHeld != 0 {
		t.Fatal("clean readable inventory did not clear the unpublished hold count", err)
	}
}

func TestHookPollPreservesApprovalCountUntilResolution(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "cursor")
	target.State, target.ApprovalHeld = model.StateApproval, 1
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
