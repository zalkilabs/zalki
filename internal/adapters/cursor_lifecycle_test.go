package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters/cursor"
	"github.com/zalkilabs/zalki/internal/bus"
)

// Retirement outcome only: a hook on a retired session may be rejected.
func cursorHookAllowingError(service *bus.Service, actor, event, status string) {
	input, _ := json.Marshal(map[string]any{"conversation_id": strings.TrimPrefix(actor, "cursor:"),
		"hook_event_name": event, "status": status})
	_ = service.Hook("cursor", bytes.NewReader(input), &bytes.Buffer{})
}

func TestCursorOnlyAnActualPromptReactivatesARetiredConversation(t *testing.T) {
	store := adapterTestStore(t)
	to, key := registerAdapterSession(t, store, "cursor")
	service := bus.Service{Store: store}
	cursorHook(t, &service, to.ID, "sessionEnd", "", 0)
	for _, event := range []struct{ name, status string }{{"postToolUse", ""}, {"stop", "completed"}} {
		cursorHookAllowingError(&service, to.ID, event.name, event.status)
		if current := adapterTestSession(t, store, to.ID); !current.Retired || current.State != bus.StateRetired {
			t.Fatalf("late %s hook reactivated a retired conversation", event.name)
		}
	}
	cursorHook(t, &service, to.ID, "beforeSubmitPrompt", "", 0)
	current := adapterTestSession(t, store, to.ID)
	if current.Retired || current.RetiredReason != "" || current.State != bus.StateBusy || current.Generation != to.Generation {
		t.Fatalf("prompt did not resume the same registration: %+v", current)
	}
	if _, err := store.Messages(to.ID, key); err != nil {
		t.Fatal("resumed conversation lost its capability", err)
	}

	unknown := "cursor:" + newTestID()
	cursorHook(t, &service, unknown, "beforeSubmitPrompt", "", 0)
	// List hides retired and exited records; check the journal directly.
	if _, err := os.Stat(filepath.Join(store.Dir, "sessions", strings.ReplaceAll(unknown, ":", "_")+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a prompt registered an unknown conversation without SessionStart", err)
	}
}

// A replacement registration gets a new generation while the previous listener
// still holds the conversation's lifetime lock. The old listener must neither
// receive the new generation's messages nor clear its state on shutdown.
func TestCursorReplacementRegistrationFencesARunningListener(t *testing.T) {
	store := adapterTestStore(t)
	from, fromKey := registerAdapterSession(t, store, "codex")
	old, oldKey := registerAdapterSession(t, store, "cursor")
	stopOld, oldOutput := startRecoveryListener(t, store, old.ID, oldKey)
	if !adapterTestSession(t, store, old.ID).Receive.IdleWake {
		t.Fatal("armed listener did not advertise idle wake")
	}

	replacement, key, err := store.Register("cursor", old.NativeID, "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Generation == old.Generation || adapterTestSession(t, store, old.ID).Receive.IdleWake {
		t.Fatal("replacement registration inherited the previous listener")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := cursor.Listen(ctx, store, old.ID, key, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already has a listener") || time.Since(started) > time.Second {
		t.Fatal("a second listener was not promptly refused for lifetime lock ownership", err)
	}
	report, err := store.Diagnostics()
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	for _, d := range report.Sessions {
		if d.ID != old.ID {
			continue
		}
		reported = true
		if d.ListenerHealth != "legacy_running" || !strings.Contains(d.RecoveryHint, "earlier Zalki build or registration") {
			t.Fatalf("doctor did not say to replace the previous registration's listener: %+v", d)
		}
	}
	if !reported {
		t.Fatal("Cursor session missing from diagnostics")
	}

	service := bus.Service{Store: store}
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: fromKey, To: old.ID, MessageID: newTestID(), Body: "for replacement"})
	if err != nil || msg.TargetGeneration != replacement.Generation || msg.Status == "wake_hint_written" {
		t.Fatalf("message reached the previous generation's listener: %+v %v", msg, err)
	}
	stopOld()
	if strings.Contains(oldOutput.text(), "ZALKI_WAKE") {
		t.Fatal("previous listener emitted a wake for the replacement generation")
	}
	current := adapterTestSession(t, store, old.ID)
	if current.Generation != replacement.Generation || current.Receive.IdleWake {
		t.Fatalf("previous listener shutdown changed the replacement registration: %+v", current)
	}
	if _, err := store.Messages(old.ID, key); err != nil {
		t.Fatal("previous listener shutdown invalidated the replacement capability", err)
	}

	_, output := startRecoveryListener(t, store, old.ID, key)
	next, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: fromKey, To: old.ID, MessageID: newTestID(), Body: "after rearm"})
	if err != nil || next.Status != "wake_hint_written" || !strings.Contains(output.text(), "ZALKI_WAKE "+next.ID) {
		t.Fatalf("replacement listener did not receive its own generation's message: %+v %v", next, err)
	}
}

// A user prompt after an aborted turn re-enables exactly one recovery, even when
// recovery was already attempted before the abort.
func TestCursorPromptAfterAbortReenablesAnAttemptedRecovery(t *testing.T) {
	store := adapterTestStore(t)
	to, _ := registerAdapterSession(t, store, "cursor")
	service := bus.Service{Store: store}
	recoveryKey(t, cursorHook(t, &service, to.ID, "postToolUse", "", 0))
	if text := cursorHook(t, &service, to.ID, "postToolUse", "", 0); text != "" {
		t.Fatal("recovery repeated within one unarmed interval")
	}
	cursorHook(t, &service, to.ID, "stop", "aborted", 0)
	cursorHook(t, &service, to.ID, "beforeSubmitPrompt", "", 0)
	recoveryKey(t, cursorHook(t, &service, to.ID, "postToolUse", "", 0))
	if text := cursorHook(t, &service, to.ID, "postToolUse", "", 0); text != "" {
		t.Fatal("resumed conversation allowed more than one recovery")
	}
}

// A crash can leave a listener lease with no listener behind it. Recovery must
// retire it with the reissued capability rather than keep advertising idle wake.
func TestCursorRecoveryClearsAStaleWake(t *testing.T) {
	store := adapterTestStore(t)
	to, _ := registerAdapterSession(t, store, "cursor")
	path := filepath.Join(store.Dir, "sessions", strings.ReplaceAll(to.ID, ":", "_")+".json")
	var record map[string]any
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &record) != nil {
		t.Fatal("unreadable session record", err)
	}
	record["schema"] = 2
	record["listener"] = map[string]any{"id": newTestID(), "generation": to.Generation, "endpoint": map[string]any{"path": "/tmp/zalki-cursor-crashed/inbox.sock", "device": 1, "inode": 1}}
	if err := writeTestJSON(path, record); err != nil {
		t.Fatal(err)
	}
	if !adapterTestSession(t, store, to.ID).Receive.IdleWake {
		t.Fatal("fixture did not record a wake endpoint")
	}
	service := bus.Service{Store: store}
	recoveryKey(t, cursorHook(t, &service, to.ID, "postToolUse", "", 0))
	if adapterTestSession(t, store, to.ID).Receive.IdleWake {
		t.Fatal("recovery kept advertising a crashed listener's wake")
	}
}

type failedListenerOutput struct{}

func (failedListenerOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A listener that cannot announce itself never completes setup, so its
// recovery guard stays in place instead of starting an automatic retry loop.
func TestCursorRecoveryOutputFailureDoesNotLoop(t *testing.T) {
	store := adapterTestStore(t)
	to, _ := registerAdapterSession(t, store, "cursor")
	service := bus.Service{Store: store}
	key := recoveryKey(t, cursorHook(t, &service, to.ID, "postToolUse", "", 0))
	err := cursor.Listen(context.Background(), store, to.ID, key, failedListenerOutput{})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected native output failure, got %v", err)
	}
	if text := cursorHook(t, &service, to.ID, "postToolUse", "", 0); text != "" {
		t.Fatal("failed task output triggered an automatic setup loop")
	}
	if current := adapterTestSession(t, store, to.ID); current.Listener != nil || current.Receive.IdleWake {
		t.Fatal("failed task still advertised a live listener")
	}
}
