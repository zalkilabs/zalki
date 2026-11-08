package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func queueThenDropCount(t *testing.T, store Store) (model.Session, model.Message, model.Message) {
	t.Helper()
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "held"})
	if err != nil || !msg.Held || msg.Status != model.StatusApprovalBlocked {
		t.Fatalf("queue: %+v %v", msg, err)
	}
	current, err := store.session(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.ApprovalHeld = 0
	if err := store.locked(func() error { return store.saveSession(current) }); err != nil {
		t.Fatal(err)
	}
	legacy := model.Message{Schema: recordSchema, ID: newID(), From: sender.ID, To: target.ID, Body: "old refusal", TargetGeneration: target.Generation, CreatedAt: model.Timestamp(), Status: model.StatusApprovalBlocked}
	if err := store.saveMessage(legacy); err != nil {
		t.Fatal(err)
	}
	return target, msg, legacy
}

func TestPruneRecountsADroppedApprovalHold(t *testing.T) {
	store := fixtureStore(t)
	target, msg, legacy := queueThenDropCount(t, store)
	untouched, _ := register(t, store, "cursor")
	before, err := os.ReadFile(mustSessionPath(t, store, untouched.ID))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := store.Prune(true, nil)
	if err != nil || !pruneReported(preview, target.ID, "approval_recounted") {
		t.Fatalf("dry-run: %+v %v", preview.Sessions, err)
	}
	if current, err := store.session(target.ID); err != nil || current.ApprovalHeld != 0 {
		t.Fatalf("dry-run restored the count: %+v %v", current.ApprovalHeld, err)
	}
	report, err := store.Prune(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pruneReported(report, target.ID, "approval_recounted") {
		t.Fatalf("report: %+v", report.Sessions)
	}
	restored, err := store.session(target.ID)
	if err != nil || restored.ApprovalHeld != 1 {
		t.Fatalf("count: %+v %v", restored.ApprovalHeld, err)
	}
	held, err := store.message(msg.ID)
	if err != nil || !held.Held || held.Status != model.StatusApprovalBlocked {
		t.Fatalf("live queue was settled: %+v %v", held, err)
	}
	savedLegacy, err := store.message(legacy.ID)
	if err != nil || savedLegacy.Held || savedLegacy.Status != model.StatusApprovalBlocked {
		t.Fatalf("legacy refusal was counted: %+v %v", savedLegacy, err)
	}
	after, err := os.ReadFile(mustSessionPath(t, store, untouched.ID))
	if err != nil || string(before) != string(after) {
		t.Fatal("prune rewrote a session with no held messages", err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	stopHook(t, service, "codex", target.ID)
	if len(delivered) != 1 || delivered[0] != "held" {
		t.Fatalf("restored queue did not release: %v", delivered)
	}
}

func TestPruneLowersAnInflatedApprovalHold(t *testing.T) {
	store := fixtureStore(t)
	target, msg, _ := queueThenDropCount(t, store)
	current, err := store.session(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.ApprovalHeld = 4
	if err := store.locked(func() error { return store.saveSession(current) }); err != nil {
		t.Fatal(err)
	}
	report, err := store.Prune(false, nil)
	if err != nil || !pruneReported(report, target.ID, "approval_recounted") {
		t.Fatalf("report: %+v %v", report.Sessions, err)
	}
	restored, err := store.session(target.ID)
	if err != nil || restored.ApprovalHeld != 1 {
		t.Fatalf("count: %+v %v", restored.ApprovalHeld, err)
	}
	held, err := store.message(msg.ID)
	if err != nil || !held.Held {
		t.Fatalf("inflated count settled the queue: %+v %v", held, err)
	}
}

func TestPruneSettlesHeldMessagesWhenTheTargetIsRetired(t *testing.T) {
	store := fixtureStore(t)
	target, msg, _ := queueThenDropCount(t, store)
	if err := store.Update(target.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	report, err := store.Prune(true, nil)
	if err != nil || !pruneReported(report, target.ID, "approval_settled") {
		t.Fatalf("dry-run: %+v %v", report.Sessions, err)
	}
	still, err := store.message(msg.ID)
	if err != nil || !still.Held {
		t.Fatalf("dry-run settled the queue: %+v %v", still, err)
	}
	report, err = store.Prune(false, nil)
	if err != nil || !pruneReported(report, target.ID, "approval_settled") {
		t.Fatalf("prune: %+v %v", report.Sessions, err)
	}
	settled, err := store.message(msg.ID)
	if err != nil || settled.Held || settled.Status != model.StatusNotSent || settled.Receipt == nil || settled.Receipt.Error != "destination has retired" {
		t.Fatalf("retired queue: %+v %v", settled, err)
	}
	current, err := store.session(target.ID)
	if err != nil || current.ApprovalHeld != 0 {
		t.Fatalf("retired count: %+v %v", current, err)
	}
}

func TestPruneSettlesHeldMessagesWhenTheTargetIsGone(t *testing.T) {
	store := fixtureStore(t)
	target, msg, _ := queueThenDropCount(t, store)
	path, err := store.sessionPath(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	report, err := store.Prune(false, nil)
	if err != nil || !pruneReported(report, target.ID, "approval_settled") {
		t.Fatalf("missing target: %+v %v", report.Sessions, err)
	}
	settled, err := store.message(msg.ID)
	if err != nil || settled.Held || settled.Status != model.StatusNotSent {
		t.Fatalf("orphaned queue: %+v %v", settled, err)
	}
}

func TestPruneReportsUnreadableMessagesAndContinues(t *testing.T) {
	store := fixtureStore(t)
	live, liveMsg, _ := queueThenDropCount(t, store)
	retired, retiredMsg, _ := queueThenDropCount(t, store)
	if err := store.Update(retired.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	old, _ := register(t, store, "cursor")
	old.Retired, old.State, old.RetiredReason = true, model.StateRetired, "session_end"
	old.UpdatedAt = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := store.locked(func() error { return store.saveSession(old) }); err != nil {
		t.Fatal(err)
	}
	id := newID()
	path := filepath.Join(store.Dir, "messages", id+".json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })

	preview, err := store.Prune(true, nil)
	if err != nil || !pruneReported(preview, id, "message_unreadable") {
		t.Fatalf("dry-run: %+v %v", preview.Sessions, err)
	}
	if current, err := store.session(live.ID); err != nil || current.ApprovalHeld != 0 {
		t.Fatalf("dry-run changed the live count: %v %v", current.ApprovalHeld, err)
	}
	if still, err := store.message(retiredMsg.ID); err != nil || !still.Held {
		t.Fatalf("dry-run settled a retired queue: %+v %v", still, err)
	}

	report, err := store.Prune(false, nil)
	if err != nil || !pruneReported(report, id, "message_unreadable") {
		t.Fatalf("prune stopped for an unreadable message: %+v %v", report.Sessions, err)
	}
	restored, err := store.session(live.ID)
	if err != nil || restored.ApprovalHeld != 1 {
		t.Fatalf("live count: %v %v", restored.ApprovalHeld, err)
	}
	held, err := store.message(liveMsg.ID)
	if err != nil || !held.Held {
		t.Fatalf("live message: %+v %v", held, err)
	}
	settled, err := store.message(retiredMsg.ID)
	if err != nil || settled.Held || settled.Status != model.StatusNotSent {
		t.Fatalf("retired message: %+v %v", settled, err)
	}
	if _, err := store.session(old.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unreadable message blocked retention", err)
	}
}

func pruneReported(report PruneReport, id, reason string) bool {
	for _, item := range report.Sessions {
		if item.ID == id && item.Reason == reason {
			return true
		}
	}
	return false
}

func mustSessionPath(t *testing.T, store Store, id string) string {
	t.Helper()
	path, err := store.sessionPath(id)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrunePreviewSettlesQueueForSelectedLiveTarget(t *testing.T) {
	store := fixtureStore(t)
	target, msg, _ := queueThenDropCount(t, store)
	preview, err := store.Prune(true, []string{target.ID})
	if err != nil || !pruneReported(preview, target.ID, "explicit_cleanup") || !pruneReported(preview, target.ID, "approval_settled") {
		t.Fatalf("dry-run missed selected retirement or its queue: %+v %v", preview, err)
	}
	current, err := store.session(target.ID)
	if err != nil || current.Retired {
		t.Fatalf("dry-run retired the target: %+v %v", current, err)
	}
	held, err := store.message(msg.ID)
	if err != nil || !held.Held {
		t.Fatalf("dry-run settled the queue: %+v %v", held, err)
	}
	applied, err := store.Prune(false, []string{target.ID})
	if err != nil || !pruneReported(applied, target.ID, "explicit_cleanup") || !pruneReported(applied, target.ID, "approval_settled") || len(applied.Sessions) != len(preview.Sessions) {
		t.Fatalf("applied retirement differs from preview: %+v %+v %v", preview, applied, err)
	}
	settled, err := store.message(msg.ID)
	if err != nil || settled.Held || settled.Status != model.StatusNotSent {
		t.Fatalf("selected retirement left held work: %+v %v", settled, err)
	}
}
