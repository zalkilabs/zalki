package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func permissionLifecycleHook(t *testing.T, service *bus.Service, target model.Session, event, source string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"session_id": target.NativeID, "hook_event_name": event, "source": source, "cwd": target.CWD})
	if err := service.Hook(t.Context(), target.Kind, bytes.NewReader(raw), io.Discard); err != nil {
		t.Fatal("permission lifecycle hook", event, err)
	}
}

func TestCodexPermissionRequestDoesNotHoldAnIdleSend(t *testing.T) {
	store, _, _ := codexEndFixture(t, false)
	sender, key := registerAdapterSession(t, store, "codex")
	target, _ := registerAdapterSession(t, store, "codex")
	calls := 0
	service := bus.Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		calls++
		return model.NewReceipt(model.StatusSubmitted)
	}}
	permissionLifecycleHook(t, &service, target, "PermissionRequest", "")
	current := adapterTestSession(t, store, target.ID)
	msg, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newTestID(), Body: "automatic review is not an open prompt"})
	if err != nil || current.State == model.StateApproval || current.Retired || msg.Held || msg.Status != model.StatusSubmitted || calls != 1 {
		t.Fatalf("Codex permission hook must not invent an approval hold: state=%s held=%v status=%s calls=%d error=%v", current.State, msg.Held, msg.Status, calls, err)
	}
}

func TestClaudePermissionRequestStillHoldsUntilStop(t *testing.T) {
	store, _, _ := codexEndFixture(t, false)
	sender, key := registerAdapterSession(t, store, "codex")
	target, _ := registerAdapterSession(t, store, "claude")
	calls := 0
	service := bus.Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		calls++
		return model.NewReceipt(model.StatusSubmitted)
	}}
	permissionLifecycleHook(t, &service, target, "PermissionRequest", "")
	msg, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newTestID(), Body: "Claude still has a real permission prompt"})
	if err != nil || !msg.IsQueuedForApproval() || calls != 0 {
		t.Fatal("Claude permission hold changed", msg.Held, msg.Status, calls, err)
	}
	permissionLifecycleHook(t, &service, target, "Stop", "")
	permissionLifecycleHook(t, &service, target, "Stop", "")
	saved := adapterTestMessage(t, store, msg.ID)
	if saved.Held || saved.Status != model.StatusSubmitted || calls != 1 {
		t.Fatal("Claude permission release changed", saved.Held, saved.Status, calls)
	}
}

func TestCodexTransportHoldNonCompactStartKeepsSettlementPolicy(t *testing.T) {
	store, _, _ := codexEndFixture(t, false)
	sender, key := registerAdapterSession(t, store, "codex")
	target, _ := registerAdapterSession(t, store, "codex")
	calls := 0
	service := bus.Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		calls++
		return model.NewReceipt(model.StatusApprovalBlocked)
	}}
	msg, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newTestID(), Body: "hold before noncompact start"})
	if err != nil || !msg.IsQueuedForApproval() {
		t.Fatal("native approval was not held before restart", msg.Held, msg.Status, err)
	}
	permissionLifecycleHook(t, &service, target, "SessionStart", "startup")
	current := adapterTestSession(t, store, target.ID)
	saved := adapterTestMessage(t, store, msg.ID)
	if current.Generation == target.Generation || current.ApprovalHeld != 0 || saved.Held || saved.Status != model.StatusNotSent || calls != 1 {
		t.Fatalf("noncompact start settlement policy changed: same_generation=%v count=%d held=%v status=%s calls=%d", current.Generation == target.Generation, current.ApprovalHeld, saved.Held, saved.Status, calls)
	}
}
