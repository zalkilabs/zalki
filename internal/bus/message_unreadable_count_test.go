package bus

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// The held-approval count after a tolerant scan: see heldAfterScan.

// holdAnother queues one more message for target's approval prompt.
func holdAnother(t *testing.T, service Service, target model.Session) model.Message {
	t.Helper()
	sender, key := register(t, service.Store, "codex")
	if err := service.Store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "another hold"})
	if err != nil || !msg.IsQueuedForApproval() {
		t.Fatalf("hold: %+v %v", msg, err)
	}
	if err := service.Store.Update(target.ID, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
	return msg
}

// breakFile truncates a message file and returns its original bytes.
func breakFile(t *testing.T, store Store, id string) []byte {
	t.Helper()
	path := filepath.Join(store.Dir, "messages", id+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original[:len(original)/2], 0600); err != nil {
		t.Fatal(err)
	}
	return original
}

func restoreFile(t *testing.T, store Store, id string, original []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", id+".json"), original, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHeldAfterScan(t *testing.T) {
	for _, c := range []struct{ prior, processed, skipped, want int }{
		{0, 0, 0, 0}, {3, 3, 0, 0}, {3, 1, 0, 0}, // healthy scan: exact count 0
		{1, 1, 1, 0}, // unrelated poison: the resolved hold is subtracted
		{2, 1, 1, 1}, // one hold may be in the unreadable file
		{2, 0, 5, 2}, // skipped files never raise the count above prior - processed
		{5, 0, 2, 2}, // and the count never exceeds skipped
		{1, 4, 3, 0}, // never negative
	} {
		if got := heldAfterScan(c.prior, c.processed, c.skipped); got != c.want {
			t.Errorf("heldAfterScan(%d, %d, %d) = %d, want %d", c.prior, c.processed, c.skipped, got, c.want)
		}
	}
}

// approvalCycle holds one message for target and releases it through the
// lifecycle hook and the hook poll. It returns the count after the cycle. A new
// sender per cycle keeps the pair flood limit out of the test.
func approvalCycle(t *testing.T, service Service, target model.Session, cycle int) (int, string) {
	t.Helper()
	sender, key := register(t, service.Store, "codex")
	if err := service.Store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "fixture approval"})
	if err != nil || !msg.IsQueuedForApproval() {
		t.Fatalf("cycle %d refused a hold: count=%d err=%v", cycle, approvalHeld(t, service.Store, target.ID), err)
	}
	if err := service.Store.Update(target.ID, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
	if err := service.releaseApprovalQueue(context.Background(), target.ID); err != nil {
		t.Fatal(err)
	}
	if text, err := service.Store.EmitPending(target.ID); err != nil || !strings.Contains(text, msg.ID) {
		t.Fatalf("cycle %d did not emit its message: %v", cycle, err)
	}
	return approvalHeld(t, service.Store, target.ID), msg.ID
}

// A file that is not a hold never ratchets the count, also over 20 cycles, and
// repairing it mid-way changes nothing for healthy cycles.
func TestUnrelatedPoisonNeverRatchetsTheHeldCount(t *testing.T) {
	for _, repairAt := range []int{0, 10} {
		service := newTestService(t)
		target, _ := register(t, service.Store, "cursor")
		bad := plant(t, service.Store, poisons()[0])
		for cycle := 1; cycle <= 20; cycle++ {
			if cycle == repairAt {
				if err := os.Remove(filepath.Join(service.Store.Dir, "messages", bad+".json")); err != nil {
					t.Fatal(err)
				}
			}
			if got, _ := approvalCycle(t, service, target, cycle); got != 0 {
				t.Fatalf("repairAt=%d cycle %d: count %d after its only hold was released", repairAt, cycle, got)
			}
		}
	}
}

// A poison that really is a hold keeps exactly one credit across cycles; every
// scan reports once; prune's exact recount clears it after the file is gone.
func TestPersistentUnreadableHoldKeepsOneCredit(t *testing.T) {
	service, target, first := heldFixture(t)
	stuck := holdAnother(t, service, target)
	breakFile(t, service.Store, stuck.ID)
	output := capture(&service.Store)
	if err := service.releaseApprovalQueue(context.Background(), target.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(emitText(t, service.Store, target.ID), first.ID) {
		t.Fatal("the readable hold was not delivered")
	}
	for cycle := 1; cycle <= 10; cycle++ {
		output.Reset()
		if got, _ := approvalCycle(t, service, target, cycle); got != 1 {
			t.Fatalf("cycle %d: count %d, want 1 for the unreadable hold", cycle, got)
		}
		// One lifecycle scan, then the hook poll's approval and pending scans.
		if records := skippedRecords(t, output); len(records) != 3 {
			t.Fatalf("cycle %d: %d diagnostics, want one per scan (3)", cycle, len(records))
		}
	}
	if err := os.Remove(filepath.Join(service.Store.Dir, "messages", stuck.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.Prune(false, nil); err != nil {
		t.Fatal(err)
	}
	if got := approvalHeld(t, service.Store, target.ID); got != 0 {
		t.Fatalf("prune left count %d after the poison was removed", got)
	}
}

func emitText(t *testing.T, store Store, id string) string {
	t.Helper()
	text, err := store.EmitPending(id)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// Independent review 1: a hold unreadable at resume is carried and, once
// readable, settled as not sent; it is never delivered to the new generation.
func TestB3ReviewRepairedHeldRecordSettlesAfterResume(t *testing.T) {
	service, target, held := heldFixture(t)
	original := breakFile(t, service.Store, held.ID)
	resumed, _, err := service.Store.Register(target.Kind, target.NativeID, "/workspace", nil)
	if err != nil || resumed.Generation == target.Generation {
		t.Fatalf("resume: %v", err)
	}
	if got := approvalHeld(t, service.Store, target.ID); got != 1 {
		t.Fatalf("resume carried %d, want 1", got)
	}
	restoreFile(t, service.Store, held.ID, original)
	if err := service.releaseApprovalQueue(context.Background(), target.ID); err != nil {
		t.Fatal(err)
	}
	if output := emitText(t, service.Store, target.ID); strings.Contains(output, held.ID) {
		t.Fatal("old generation message emitted")
	}
	stored := storedMessage(t, service.Store, held.ID)
	if stored.IsQueuedForApproval() || stored.Status != model.StatusNotSent {
		t.Fatalf("repaired old-generation message unresolved: %s held=%t", stored.Status, stored.Held)
	}
	if got := approvalHeld(t, service.Store, target.ID); got != 0 {
		t.Fatalf("count %d after the carried hold was settled", got)
	}
}

// Independent review 2: one unrelated unreadable file must not exhaust capacity.
func TestB3ReviewUnrelatedUnreadableRecordDoesNotExhaustApprovalCapacity(t *testing.T) {
	service := newTestService(t)
	target, _ := register(t, service.Store, "cursor")
	plant(t, service.Store, poisons()[0])
	for cycle := 1; cycle <= approvalQueueCap+1; cycle++ {
		if _, id := approvalCycle(t, service, target, cycle); storedMessage(t, service.Store, id).IsQueuedForApproval() {
			t.Fatalf("cycle %d left a held record", cycle)
		}
	}
}

// Generation rollover: readable holds are settled and subtracted; only holds
// that may sit in unreadable files are carried, never above the queue cap.
func TestResumeCarriesOnlyUnreadableHoldsWithinTheCap(t *testing.T) {
	t.Run("unrelated poison", func(t *testing.T) {
		service, target, _ := heldFixture(t)
		holdAnother(t, service, target)
		plant(t, service.Store, poisons()[0])
		resumeAndRequireCarried(t, service, target, 0)
	})
	t.Run("unreadable hold", func(t *testing.T) {
		service, target, _ := heldFixture(t)
		stuck := holdAnother(t, service, target)
		breakFile(t, service.Store, stuck.ID)
		resumeAndRequireCarried(t, service, target, 1)
	})
	t.Run("cap", func(t *testing.T) {
		service, target, first := heldFixture(t)
		ids := []string{first.ID}
		for len(ids) < approvalQueueCap {
			ids = append(ids, holdAnother(t, service, target).ID)
		}
		for _, id := range ids {
			breakFile(t, service.Store, id)
		}
		for range 4 {
			plant(t, service.Store, poisons()[0])
		}
		resumeAndRequireCarried(t, service, target, approvalQueueCap)
	})
}

func resumeAndRequireCarried(t *testing.T, service Service, target model.Session, want int) {
	t.Helper()
	if _, _, err := service.Store.Register(target.Kind, target.NativeID, "/workspace", nil); err != nil {
		t.Fatal(err)
	}
	if got := approvalHeld(t, service.Store, target.ID); got != want {
		t.Fatalf("resume carried %d, want %d", got, want)
	}
}
