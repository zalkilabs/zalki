package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func approvalService(t *testing.T, store Store, delivered *[]string) Service {
	t.Helper()
	return Service{Store: store, Deliver: func(_ context.Context, msg model.Message, _ model.Session) model.Receipt {
		*delivered = append(*delivered, msg.Body)
		return model.NewReceipt(model.StatusSubmitted)
	}}
}

func stopHook(t *testing.T, service Service, kind, id string) {
	t.Helper()
	input, _ := json.Marshal(map[string]any{
		"session_id": strings.TrimPrefix(id, kind+":"), "conversation_id": strings.TrimPrefix(id, kind+":"),
		"hook_event_name": "Stop", "status": "completed",
	})
	if err := service.Hook(t.Context(), kind, bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalQueueDeliversOnceInOrder(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	bodies := []string{"one", "two", "three"}
	var first model.Message
	for i, body := range bodies {
		msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: body})
		if err != nil || msg.Status != model.StatusApprovalBlocked || msg.Receipt == nil || msg.Receipt.Error != model.ApprovalQueuedNote {
			t.Fatalf("queued send %d: %+v %v", i, msg, err)
		}
		if i == 0 {
			first = msg
		}
	}
	if len(delivered) != 0 {
		t.Fatal("transport ran while the permission prompt was open")
	}
	again, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: first.ID, Body: first.Body})
	if err != nil || again.Status != model.StatusApprovalBlocked || len(delivered) != 0 {
		t.Fatal("retry delivered a queued message", again, err)
	}
	held, err := store.session(target.ID)
	if err != nil || held.ApprovalHeld != len(bodies) {
		t.Fatalf("queue count: %+v %v", held.ApprovalHeld, err)
	}
	stopHook(t, service, "codex", target.ID)
	if strings.Join(delivered, ",") != "one,two,three" {
		t.Fatalf("delivery order: %v", delivered)
	}
	stopHook(t, service, "codex", target.ID)
	if len(delivered) != 3 {
		t.Fatal("a second stop delivered again", delivered)
	}
	saved, err := store.message(first.ID)
	if err != nil || saved.Status != model.StatusSubmitted {
		t.Fatalf("receipt: %+v %v", saved, err)
	}
	cleared, err := store.session(target.ID)
	if err != nil || cleared.ApprovalHeld != 0 {
		t.Fatalf("queue count after delivery: %d %v", cleared.ApprovalHeld, err)
	}
}

func TestFreshRegistrationSettlesHeldMessages(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "held"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.register(target.Kind, model.NativeID(target), target.CWD, nil, false); err != nil {
		t.Fatal(err)
	}
	saved, err := store.message(msg.ID)
	if err != nil || saved.Status != model.StatusNotSent || saved.Receipt == nil || !strings.Contains(saved.Receipt.Error, "registration changed") {
		t.Fatalf("fresh registration left the message queued: %+v %v", saved, err)
	}
	current, err := store.session(target.ID)
	if err != nil || current.ApprovalHeld != 0 {
		t.Fatalf("count after fresh registration: %d %v", current.ApprovalHeld, err)
	}
	stopHook(t, service, "codex", target.ID)
	if len(delivered) != 0 {
		t.Fatal("settled message was delivered", delivered)
	}
}

func TestCompactRegistrationKeepsHeldMessages(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "kept"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.register(target.Kind, model.NativeID(target), target.CWD, nil, true); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(target.ID)
	if err != nil || current.ApprovalHeld != 1 || current.State != model.StateApproval {
		t.Fatalf("compaction dropped the queue: held=%d state=%s %v", current.ApprovalHeld, current.State, err)
	}
	stopHook(t, service, "codex", target.ID)
	if len(delivered) != 1 || delivered[0] != "kept" {
		t.Fatalf("delivery after compaction: %v", delivered)
	}
}

func TestLegacyApprovalRefusalIsNotReleased(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	legacy := model.Message{Schema: recordSchema, ID: newID(), From: sender.ID, To: target.ID, Body: "old refusal", TargetGeneration: target.Generation, CreatedAt: model.Timestamp(), Status: model.StatusApprovalBlocked}
	if err := store.saveMessage(legacy); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "new hold"}); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(target.ID)
	if err != nil || current.ApprovalHeld != 1 {
		t.Fatalf("legacy refusal counted toward the queue: %d %v", current.ApprovalHeld, err)
	}
	stopHook(t, service, "codex", target.ID)
	if len(delivered) != 1 || delivered[0] != "new hold" {
		t.Fatalf("delivery: %v", delivered)
	}
	saved, err := store.message(legacy.ID)
	if err != nil || saved.Status != model.StatusApprovalBlocked || saved.Held || saved.Body != "old refusal" {
		t.Fatalf("legacy record changed: %+v %v", saved, err)
	}
}

func TestHookDoesNotReadMessagesWhenNothingIsQueued(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	target, _ := register(t, store, "codex")
	path := filepath.Join(store.Dir, "messages", "corrupt.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}

	stopHook(t, service, "codex", target.ID)
}

func TestApprovalQueueCapsAndDropsOnRetirement(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	for range approvalQueueCap {
		if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "held"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "overflow"}); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatal("cap accepted another message", err)
	}
	input, _ := json.Marshal(map[string]any{
		"session_id": strings.TrimPrefix(target.ID, "codex:"), "hook_event_name": "SessionEnd",
	})
	if err := service.Hook(t.Context(), "codex", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 0 {
		t.Fatal("retired session was delivered", delivered)
	}
	queued, err := store.queuedApprovals(target.ID)
	if err != nil || len(queued) != 0 {
		t.Fatal("approval queue survived retirement", queued, err)
	}
}

func TestApprovalQueueDropsWhenTheOwnerHasExited(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "held"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.session(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Owner = &model.ProcessRef{PID: 1 << 30, Start: "1", Role: "backend"}
	if err := store.saveSession(current); err != nil {
		t.Fatal(err)
	}
	stopHook(t, service, "codex", target.ID)
	if len(delivered) != 0 {
		t.Fatal("exited session was delivered", delivered)
	}
	saved, err := store.message(msg.ID)
	if err != nil || saved.Status != model.StatusNotSent || saved.Receipt == nil || !strings.Contains(saved.Receipt.Error, "exited") {
		t.Fatalf("exit receipt: %+v %v", saved, err)
	}
}

func TestApprovalQueueFeedsHookDeliveryOnce(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	sender, key, target, _ := registerPair(t, store, "codex", "cursor")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}

	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "through the hook"})
	if err != nil || msg.Status != model.StatusApprovalBlocked {
		t.Fatal(msg, err)
	}
	input, _ := json.Marshal(map[string]any{
		"conversation_id": strings.TrimPrefix(target.ID, "cursor:"), "hook_event_name": "postToolUse",
	})
	if err := service.Hook(t.Context(), "cursor", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	text, err := store.EmitPending(target.ID)
	if err != nil || !strings.Contains(text, "through the hook") {
		t.Fatalf("hook context: %q %v", text, err)
	}
	again, err := store.EmitPending(target.ID)
	if err != nil || again != "" {
		t.Fatalf("emitted twice: %q %v", again, err)
	}
}

func TestApprovalQueueDeliversANoticeAfterThePrompt(t *testing.T) {
	store := fixtureStore(t)
	watcher, key, target, _ := registerPair(t, store, "codex", "claude")
	if err := store.Update(watcher.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	if _, err := service.Watch(watcher.ID, key, target.ID); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{
		"session_id": strings.TrimPrefix(target.ID, "claude:"), "hook_event_name": "Stop",
	})
	if err := service.Hook(t.Context(), "claude", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 0 {
		t.Fatal("notice delivered while the watcher was in approval", delivered)
	}
	stopHook(t, service, "codex", watcher.ID)
	if len(delivered) != 1 || !strings.Contains(delivered[0], "Zalki notice:") {
		t.Fatalf("notice: %v", delivered)
	}
}

func TestApprovalQueueDeliversOnceUnderConcurrentHooks(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	sender, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		n.Add(1)
		return model.NewReceipt(model.StatusSubmitted)
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "once"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			input, _ := json.Marshal(map[string]any{
				"session_id": strings.TrimPrefix(target.ID, "codex:"), "hook_event_name": "Stop", "status": "completed",
			})
			if err := service.Hook(t.Context(), "codex", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("delivered %d times", n.Load())
	}
}
