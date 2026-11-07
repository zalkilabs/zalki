package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// These tests exercise hook failures and rate-limited sends through the logger. Two stages ("watch" and "approval") and the code
// rate_limited were set by the code but missing from the logger's lists, so
// those failures were logged without a stage, or as "internal".

// hookFailed runs a hook that must fail and returns the attributes of its
// hook.failed log record.
func hookFailed(t *testing.T, service Service, handler *captureHandler, kind string, input map[string]any) []string {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Hook(t.Context(), kind, bytes.NewReader(data), &bytes.Buffer{}); err == nil {
		t.Fatal("the hook was expected to fail")
	}
	for _, record := range handler.records {
		if record.event == "hook.failed" {
			return record.attrs
		}
	}
	t.Fatalf("no hook.failed record: %+v", handler.records)
	return nil
}

func loggedService(t *testing.T) (Service, *captureHandler) {
	t.Helper()
	service := newTestService(t)
	handler := newCaptureHandler(slog.LevelDebug)
	service.Store.Logger = slog.New(handler)
	return service, handler
}

func TestAFailedWatchNotificationIsLoggedWithItsStage(t *testing.T) {
	service, handler := loggedService(t)
	_, _, target := watchedTarget(t, service, selfOwner(t))
	watches, err := filepath.Glob(filepath.Join(service.Store.Dir, "watches", "*", "*.json"))
	if err != nil || len(watches) != 1 {
		t.Fatalf("setup: watch records %v %v", watches, err)
	}
	if err := os.WriteFile(watches[0], []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	attrs := hookFailed(t, service, handler, "cursor", map[string]any{
		"conversation_id": strings.TrimPrefix(target.ID, "cursor:"), "hook_event_name": "stop", "status": "completed",
	})
	if !slices.Contains(attrs, "hook_stage=watch") {
		t.Fatalf("hook.failed lacks the watch stage: %v", attrs)
	}
}

func TestAFailedApprovalReleaseIsLoggedWithItsStage(t *testing.T) {
	service, handler := loggedService(t)
	store := service.Store
	sender, key, target, _ := registerPair(t, store, "codex", "cursor")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	if msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "held"}); err != nil || !msg.Held {
		t.Fatalf("setup: %+v %v", msg, err)
	}
	// The release scans the journal; an unreadable record makes it fail.
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", newID()+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	attrs := hookFailed(t, service, handler, "cursor", map[string]any{
		"conversation_id": strings.TrimPrefix(target.ID, "cursor:"), "hook_event_name": "postToolUse",
	})
	if !slices.Contains(attrs, "hook_stage=approval") {
		t.Fatalf("hook.failed lacks the approval stage: %v", attrs)
	}
}

func TestARateLimitedSendIsLoggedAsRateLimited(t *testing.T) {
	service, handler := loggedService(t)
	store := service.Store
	sender, key, target, _ := registerPair(t, store, "codex", "claude")
	service.Deliver = func(context.Context, Message, Session) Receipt { return NewReceipt(model.StatusSubmitted) }
	var limited bool
	for range 12 + 3 {
		_, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "again"})
		if ErrorCodeOf(err) == model.CodeRateLimited {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("the send was never rate limited")
	}
	for _, record := range handler.records {
		if slices.Contains(record.attrs, "error_code=rate_limited") {
			return
		}
	}
	t.Fatalf("no record carries error_code=rate_limited: %+v", handler.records)
}
