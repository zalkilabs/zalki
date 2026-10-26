package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

func recoveryKey(t *testing.T, text string) string {
	t.Helper()
	match := regexp.MustCompile(`refreshed private capability is ([a-f0-9-]+);`).FindStringSubmatch(text)
	if len(match) != 2 {
		t.Fatal("missing receiver-only refreshed capability in recovery context")
	}
	return match[1]
}

func cursorHook(t *testing.T, service *bus.Service, actor, event, status string, loop int) string {
	t.Helper()
	input, _ := json.Marshal(map[string]any{"conversation_id": strings.TrimPrefix(actor, "cursor:"),
		"hook_event_name": event, "status": status, "loop_count": loop})
	var out bytes.Buffer
	if err := service.Hook("cursor", bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestCursorRecoveryEmitsOnceWithoutLosingPendingMessages(t *testing.T) {
	store := adapterTestStore(t)
	from, key := registerAdapterSession(t, store, "codex")
	to, oldKey := registerAdapterSession(t, store, "cursor")
	service := bus.Service{Store: store}
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newTestID(), Body: "pending peer note"})
	if err != nil {
		t.Fatal(err)
	}
	text := cursorHook(t, &service, to.ID, "postToolUse", "", 0)
	if !strings.Contains(text, "pending peer note") || !strings.Contains(text, "additional_context") {
		t.Fatal("recovery displaced pending peer context")
	}
	newKey := recoveryKey(t, text)
	_, err = store.Messages(to.ID, newKey)
	current := adapterTestSession(t, store, to.ID)
	if err != nil || current.Generation != to.Generation || current.ID != to.ID || current.Receive.IdleWake {
		t.Fatalf("recovery changed conversation identity or advertised an unarmed listener: %v", err)
	}
	if _, err := store.Messages(to.ID, oldKey); err == nil {
		t.Fatal("old capability still valid after refresh")
	}
	stored := adapterTestMessage(t, store, msg.ID)
	if stored.TargetGeneration != to.Generation || stored.AcknowledgedAt != "" {
		t.Fatal("recovery lost pending message scope or manufactured an acknowledgement", err)
	}
	if _, err := store.Ack(to.ID, newKey, msg.ID); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"postToolUse", "stop", "stop"} {
		if text := cursorHook(t, &service, to.ID, event, "completed", 1); text != "" {
			t.Fatal("recovery repeated while listener setup was unconfirmed or declined")
		}
	}
	// Ordinary subsequent prompts must not create a permission-refusal retry loop.
	cursorHook(t, &service, to.ID, "beforeSubmitPrompt", "", 0)
	if text := cursorHook(t, &service, to.ID, "stop", "completed", 0); text != "" {
		t.Fatal("repeated recovery on a later ordinary turn without a new listener loss")
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(items)
	if bytes.Contains(b, []byte(newKey)) || bytes.Contains(b, []byte("cursor_recovery_")) {
		t.Fatal("discovery exposed private recovery state")
	}
}

func TestCursorRecoveryCompletedStopAndAbortBoundaries(t *testing.T) {
	for _, status := range []string{"aborted", "error", "completed"} {
		t.Run(status, func(t *testing.T) {
			store := adapterTestStore(t)
			to, key := registerAdapterSession(t, store, "cursor")
			service := bus.Service{Store: store}
			loop := 0
			if status == "completed" {
				loop = 5 // Provider's follow-up cap must not be bypassed.
			}
			if text := cursorHook(t, &service, to.ID, "stop", status, loop); text != "" {
				t.Fatal("recovery continued an aborted/error/capped turn")
			}
			if text := cursorHook(t, &service, to.ID, "postToolUse", "", 0); text != "" {
				t.Fatal("late tool hook resumed a suppressed recovery")
			}
			if _, err := store.Messages(to.ID, key); err != nil {
				t.Fatal("suppression unnecessarily rotated credentials", err)
			}
			cursorHook(t, &service, to.ID, "beforeSubmitPrompt", "", 0)
			text := cursorHook(t, &service, to.ID, "stop", "completed", 0)
			if !strings.Contains(text, "followup_message") {
				t.Fatal("did not recover after actual prompt resumed the conversation")
			}
			recoveryKey(t, text)
			if again := cursorHook(t, &service, to.ID, "stop", "completed", 1); again != "" {
				t.Fatal("stop recovery created an automatic model loop")
			}
		})
	}
}

func startRecoveryListener(t *testing.T, store bus.Store, actor, key string) (func(), *wakeOutput) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	out := &wakeOutput{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- store.ListenCursor(ctx, actor, key, out) }()
	select {
	case err := <-done:
		cancel()
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
		}
		t.Fatal("listener startup failed", err)
	case <-ctx.Done():
		cancel()
		t.Fatal("listener startup timed out")
	case <-out.ready:
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("listener shutdown failed", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("listener shutdown timed out")
			}
		})
	}
	t.Cleanup(stop)
	return stop, out
}

func TestCursorRecoveryRearmsAfterListenerShutdown(t *testing.T) {
	store := adapterTestStore(t)
	from, fromKey := registerAdapterSession(t, store, "codex")
	to, oldKey := registerAdapterSession(t, store, "cursor")
	service := bus.Service{Store: store}
	text := cursorHook(t, &service, to.ID, "postToolUse", "", 0)
	key := recoveryKey(t, text)
	stop, _ := startRecoveryListener(t, store, to.ID, key)
	if text := cursorHook(t, &service, to.ID, "postToolUse", "", 0); text != "" {
		t.Fatal("attempted recovery while the native listener held its live lock")
	}
	if _, err := store.Messages(to.ID, key); err != nil {
		t.Fatal("healthy listener check changed its capability", err)
	}
	stop()
	text = cursorHook(t, &service, to.ID, "postToolUse", "", 0)
	key = recoveryKey(t, text)
	_, output := startRecoveryListener(t, store, to.ID, key)
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: fromKey, To: to.ID, MessageID: newTestID(), Body: "after restart"})
	if err != nil || msg.Status != "wake_hint_written" || msg.TargetGeneration != to.Generation {
		t.Fatalf("recovered listener did not receive the original conversation's message: %+v %v", msg, err)
	}
	if !strings.Contains(output.text(), "ZALKI_WAKE "+msg.ID) {
		t.Fatal("replacement listener did not emit a wake hint")
	}
	if _, err := store.Ack(to.ID, key, msg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Messages(to.ID, oldKey); err == nil {
		t.Fatal("pre-recovery capability remained valid")
	}
}
