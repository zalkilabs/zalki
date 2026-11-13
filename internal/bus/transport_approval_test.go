package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestTransportApprovalWaitsForFreshDelivery(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	waiting, checks, submits := true, 0, 0
	service := Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		checks++
		if waiting {
			return model.NewReceipt(model.StatusApprovalBlocked)
		}
		submits++
		return model.NewReceipt(model.StatusSubmitted)
	}}
	request := SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "live approval hold"}
	msg, err := service.Send(t.Context(), request)
	assertTransportApprovalQueued(t, msg, err)
	assertTransportApprovalCount(t, store, target.ID, 1)
	if _, err := service.Send(t.Context(), request); err != nil || checks != 1 {
		t.Fatal("duplicate send retried held transport", checks, err)
	}
	for range 3 {
		stopHook(t, service, "codex", target.ID)
		assertTransportApprovalCount(t, store, target.ID, 1)
	}
	if submits != 0 || checks != 4 {
		t.Fatal("still-waiting Stop submitted or skipped fresh checks", submits, checks)
	}
	waiting = false
	stopHook(t, service, "codex", target.ID)
	stopHook(t, service, "codex", target.ID)
	assertTransportApprovalCount(t, store, target.ID, 0)
	saved, err := store.message(msg.ID)
	if err != nil || saved.Held || saved.Status != model.StatusSubmitted || submits != 1 || checks != 5 {
		t.Fatal("cleared live approval must submit exactly once", submits, checks, saved.Status, saved.Held, err)
	}
}

func assertTransportApprovalQueued(t *testing.T, msg model.Message, err error) {
	t.Helper()
	if err != nil || !msg.IsQueuedForApproval() || msg.Receipt == nil || msg.Receipt.Error != model.ApprovalQueuedNote {
		t.Fatalf("live approval must be a durable queued hold: status=%s held=%v error=%v", msg.Status, msg.Held, err)
	}
}

func assertTransportApprovalCount(t *testing.T, store Store, actor string, want int) {
	t.Helper()
	current, err := store.session(actor)
	if err != nil || current.ApprovalHeld != want {
		t.Fatalf("durable approval count=%d want=%d error=%v", current.ApprovalHeld, want, err)
	}
}

type transportApprovalResult struct {
	message model.Message
	err     error
}

func pauseTransportApproval(t *testing.T, service *Service, request SendRequest) (<-chan transportApprovalResult, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan transportApprovalResult, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		close(entered)
		select {
		case <-resume:
		case <-ctx.Done():
		}
		return model.NewReceipt(model.StatusApprovalBlocked)
	}
	t.Cleanup(func() { cancel(); release(); <-finished })
	go func() {
		defer close(finished)
		msg, err := service.Send(ctx, request)
		result <- transportApprovalResult{msg, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("transport did not reach the paused native attempt", ctx.Err())
	}
	return result, release
}

func TestTransportApprovalPreservesConcurrentFinalOutcome(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status model.DeliveryStatus
		ack    bool
	}{
		{"final-refusal", model.StatusApprovalBlocked, false},
		{"submitted", model.StatusSubmitted, false},
		{"acknowledged", model.StatusSubmitted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := fixtureStore(t)
			sender, key, target, targetKey := registerPair(t, store, "cursor", "codex")
			service := Service{Store: store}
			request := SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "concurrent final outcome"}
			result, release := pauseTransportApproval(t, &service, request)
			receipt := model.NewReceipt(tc.status)
			receipt.Error = "concurrent authoritative final result"
			if _, err := store.Finish(request.MessageID, receipt); err != nil {
				t.Fatal(err)
			}
			if tc.ack {
				if _, err := store.Ack(target.ID, targetKey, request.MessageID); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(store.Dir, "messages", request.MessageID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			release()
			got := <-result
			after, err := os.ReadFile(path)
			if got.err != nil || err != nil || got.message.Held || string(before) != string(after) {
				t.Fatalf("late approval must preserve the concurrent final record: held=%v status=%s errors=%v/%v unchanged=%v", got.message.Held, got.message.Status, got.err, err, string(before) == string(after))
			}
			assertTransportApprovalCount(t, store, target.ID, 0)
		})
	}
}

func TestTransportApprovalDoesNotCountAnExistingHoldTwice(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	service := Service{Store: store}
	request := SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "concurrent existing hold"}
	result, release := pauseTransportApproval(t, &service, request)
	if err := store.locked(func() error {
		msg, err := store.message(request.MessageID)
		if err != nil {
			return err
		}
		if err := store.holdForApproval(&msg); err != nil {
			return err
		}
		return store.saveReservation(msg)
	}); err != nil {
		t.Fatal(err)
	}
	assertTransportApprovalCount(t, store, target.ID, 1)
	release()
	got := <-result
	if got.err != nil || !got.message.IsQueuedForApproval() {
		t.Fatal("late approval did not preserve the existing held result", got.message.Status, got.message.Held, got.err)
	}
	assertTransportApprovalCount(t, store, target.ID, 1)
}

func TestTransportApprovalRefusesChangedRecipient(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "retirement"}[retire], func(t *testing.T) {
			store := fixtureStore(t)
			sender, key, target, _ := registerPair(t, store, "cursor", "codex")
			service := Service{Store: store}
			request := SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "recipient changes during live check"}
			result, release := pauseTransportApproval(t, &service, request)
			if retire {
				if err := store.Update(target.ID, model.StateRetired, true); err != nil {
					t.Fatal(err)
				}
			} else if _, _, err := store.Register(target.Kind, target.NativeID, target.CWD, nil); err != nil {
				t.Fatal(err)
			}
			release()
			got := <-result
			if got.err != nil || got.message.Held || got.message.Status != model.StatusNotSent || got.message.Receipt == nil || got.message.Receipt.Code != model.CodeUnavailable {
				t.Fatalf("changed recipient must refuse without a hold: held=%v status=%s error=%v", got.message.Held, got.message.Status, got.err)
			}
			assertTransportApprovalCount(t, store, target.ID, 0)
		})
	}
}

func TestTransportApprovalRejectsChangedAttemptIdentity(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	service := Service{Store: store}
	request := SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "attempted body"}
	result, release := pauseTransportApproval(t, &service, request)
	if err := store.locked(func() error {
		msg, err := store.message(request.MessageID)
		if err != nil {
			return err
		}
		msg.Body = "different durable message identity"
		return store.saveMessage(msg)
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "messages", request.MessageID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	release()
	got := <-result
	after, err := os.ReadFile(path)
	if model.ErrorCodeOf(got.err) != model.CodeConflict || err != nil || string(before) != string(after) {
		t.Fatalf("native approval must not overwrite a changed attempt identity: code=%s unchanged=%v error=%v", model.ErrorCodeOf(got.err), string(before) == string(after), err)
	}
	assertTransportApprovalCount(t, store, target.ID, 0)
}

func TestTransportApprovalCapacityRefusesWithoutGrowingCount(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	service := Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		return model.NewReceipt(model.StatusApprovalBlocked)
	}}
	for range approvalQueueCap {
		msg, err := service.Send(t.Context(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "bounded native approval queue"})
		if err != nil || !msg.IsQueuedForApproval() {
			t.Fatal("native approval queue filled too early", msg.Status, msg.Held, err)
		}
	}
	msg, err := service.Send(t.Context(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "over native approval capacity"})
	if err != nil || msg.Held || msg.Status != model.StatusNotSent || msg.Receipt == nil || msg.Receipt.Code != model.CodeUnavailable {
		t.Fatalf("full native approval queue must refuse: held=%v status=%s error=%v", msg.Held, msg.Status, err)
	}
	assertTransportApprovalCount(t, store, target.ID, approvalQueueCap)
}

func TestTransportApprovalSaveFailureDoesNotClaimAHold(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	service := Service{Store: store}
	request := SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "private read-only save refusal"}
	result, release := pauseTransportApproval(t, &service, request)
	// Refuse intent creation, rather than a target write after intent publication.
	dir := filepath.Join(store.Dir, "commits")
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	release()
	got := <-result
	if got.err == nil || got.message.IsQueuedForApproval() {
		t.Fatalf("failed native hold save must return an error, not a held success: status=%s held=%v error=%v", got.message.Status, got.message.Held, got.err)
	}
	if _, pendingErr := os.Stat(filepath.Join(dir, "pending.json")); !errors.Is(pendingErr, os.ErrNotExist) {
		t.Fatal("pre-intent refusal unexpectedly published an intent", pendingErr)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	assertTransportApprovalCount(t, store, target.ID, 0)
}

func TestTransportUncertainWriteDoesNotBecomeApprovalHold(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusUnknownWrite, model.StatusUnknownTimeout} {
		t.Run(string(status), func(t *testing.T) {
			store := fixtureStore(t)
			sender, key, target, _ := registerPair(t, store, "cursor", "codex")
			calls := 0
			service := Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
				calls++
				r := model.NewReceipt(status)
				r.Error = "fixture ambiguous native write"
				return r
			}}
			msg, err := service.Send(t.Context(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "uncertain native attempt"})
			if err != nil || msg.Held || msg.Status != status {
				t.Fatalf("uncertain native write must never become a retryable hold: held=%v status=%s want=%s error=%v", msg.Held, msg.Status, status, err)
			}
			stopHook(t, service, "codex", target.ID)
			if calls != 1 {
				t.Fatal("uncertain native write was automatically retried", calls)
			}
			assertTransportApprovalCount(t, store, target.ID, 0)
		})
	}
}
