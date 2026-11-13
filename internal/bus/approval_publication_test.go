package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestApprovalLatePublicationErrorKeepsCountAndReleases(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		t.Run(kind, func(t *testing.T) { checkLateApprovalPublication(t, kind) })
	}
}

func checkLateApprovalPublication(t *testing.T, kind string) {
	t.Helper()
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", kind)
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "late publication error still owns a queued message"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	late := errors.New("injected return error after real batch publication")
	err = publishedApprovalThenError(t, store, msg, late)
	if !errors.Is(err, late) {
		t.Fatal("late publication error was suppressed", err)
	}
	saved, readErr := store.message(msg.ID)
	if readErr != nil || !saved.IsQueuedForApproval() {
		t.Fatal("late control did not publish the real held record", saved.Status, saved.Held, readErr)
	}
	current, readErr := store.session(target.ID)
	if readErr != nil || current.ApprovalHeld != 1 {
		t.Errorf("durable held reservation must keep conservative count after late error: count=%d error=%v", current.ApprovalHeld, readErr)
	}
	submits := 0
	service := Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		submits++
		return model.NewReceipt(model.StatusSubmitted)
	}}
	stopHook(t, service, kind, target.ID)
	stopHook(t, service, kind, target.ID)
	saved, readErr = store.message(msg.ID)
	if readErr != nil || saved.Held || saved.Status != model.StatusSubmitted || submits != 1 {
		t.Errorf("late publication hold must release exactly once on a later hook: held=%v status=%s submits=%d error=%v", saved.Held, saved.Status, submits, readErr)
	}
	assertTransportApprovalCount(t, store, target.ID, 0)
}

func TestApprovalPendingIntentErrorKeepsCountUntilRecovery(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "pending intent must retain hold count"})
	if err != nil {
		t.Fatal(err)
	}
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.Dir, "messages")
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	err = store.locked(func() error { return store.saveReservation(msg) })
	if err == nil {
		t.Fatal("target publication refusal did not fail")
	}
	if _, pendingErr := os.Stat(filepath.Join(store.Dir, "commits", "pending.json")); pendingErr != nil {
		t.Fatal("target error did not retain a replayable intent", pendingErr)
	}
	assertTransportApprovalCount(t, store, target.ID, 1)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	submits := 0
	service := Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		submits++
		return model.NewReceipt(model.StatusSubmitted)
	}}
	stopHook(t, service, "codex", target.ID)
	stopHook(t, service, "codex", target.ID)
	saved, err := store.message(msg.ID)
	if err != nil || saved.Held || saved.Status != model.StatusSubmitted || submits != 1 {
		t.Fatal("recovered intent did not release exactly once", saved.Held, saved.Status, submits, err)
	}
	assertTransportApprovalCount(t, store, target.ID, 0)
}

func publishedApprovalThenError(t *testing.T, store Store, msg model.Message, late error) error {
	t.Helper()
	return store.locked(func() error {
		return saveReservationWith(store, msg, func(m model.Message) error {
			if err := store.saveMessageRecord(m); err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(store.Dir, "commits", "pending.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("real publication left a pending intent", err)
			}
			return late
		})
	})
}

func TestApprovalUnreadableInventoryKeepsConservativeCount(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "unknown hold inventory"})
	if err != nil {
		t.Fatal(err)
	}
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(store.Dir, "messages", newID()+".json")
	original := errors.New("injected nonpublication with unreadable hold inventory")
	err = store.locked(func() error {
		return saveReservationWith(store, msg, func(model.Message) error {
			if err := os.WriteFile(unknown, []byte("{"), 0600); err != nil {
				return err
			}
			return original
		})
	})
	if !errors.Is(err, original) {
		t.Fatal("original reservation error was not preserved", err)
	}
	current, readErr := store.session(target.ID)
	if readErr != nil || current.ApprovalHeld != 1 {
		t.Fatalf("unreadable hold inventory must keep conservative count: count=%d error=%v", current.ApprovalHeld, readErr)
	}
	if err := os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		t.Error("unpublished hold must not submit")
		return model.NewReceipt(model.StatusSubmitted)
	}}
	stopHook(t, service, "codex", target.ID)
	assertTransportApprovalCount(t, store, target.ID, 0)
}
