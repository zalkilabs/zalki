package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func recoveryKey(t *testing.T, text string) string {
	t.Helper()
	match := regexp.MustCompile(`refreshed private capability is ([a-f0-9-]+);`).FindStringSubmatch(text)
	if len(match) != 2 {
		t.Fatal("missing receiver-only refreshed capability in recovery context")
	}
	return match[1]
}

func cursorHook(t *testing.T, service *Service, actor, event, status string, loop int) string {
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
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, oldKey := register(t, store, "cursor")
	service := Service{Store: store}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "pending peer note"})
	if err != nil {
		t.Fatal(err)
	}
	text := cursorHook(t, &service, to.ID, "postToolUse", "", 0)
	if !strings.Contains(text, "pending peer note") || !strings.Contains(text, "additional_context") {
		t.Fatal("recovery displaced pending peer context")
	}
	newKey := recoveryKey(t, text)
	current, err := store.authenticate(to.ID, newKey)
	if err != nil || current.Generation != to.Generation || current.ID != to.ID || current.Receive.IdleWake {
		t.Fatalf("recovery changed conversation identity or advertised an unarmed listener: %v", err)
	}
	if _, err := store.authenticate(to.ID, oldKey); err == nil {
		t.Fatal("old capability still valid after refresh")
	}
	stored, err := store.message(msg.ID)
	if err != nil || stored.TargetGeneration != to.Generation || stored.AcknowledgedAt != "" {
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
			store := fixtureStore(t)
			to, key := register(t, store, "cursor")
			service := Service{Store: store}
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
			if _, err := store.authenticate(to.ID, key); err != nil {
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

func TestCursorRecoverySkipsRetiredUnknownAndApprovalSessions(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	for _, state := range []string{"approval", "retired"} {
		if err := store.Update(to.ID, SessionState(state), state == "retired"); err != nil {
			t.Fatal(err)
		}
		if text, err := store.cursorRecoveryContext(to.ID); err != nil || text != "" {
			t.Fatalf("recovered a %s session: %v", state, err)
		}
	}
	if text, err := store.cursorRecoveryContext("cursor:" + newID()); err != nil || text != "" {
		t.Fatal("recovery registered an unknown session", err)
	}
	current, err := store.session(to.ID)
	if err != nil || current.KeyHash != to.KeyHash {
		t.Fatal("skipped recovery changed credentials", err)
	}
}

func TestConcurrentCursorRecoveryClaimsOnlyOneContext(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	var wg sync.WaitGroup
	contexts := make(chan string, 12)
	errors := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, err := store.cursorRecoveryContext(to.ID)
			contexts <- text
			errors <- err
		}()
	}
	wg.Wait()
	close(contexts)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for text := range contexts {
		if text != "" {
			count++
			if _, err := store.authenticate(to.ID, recoveryKey(t, text)); err != nil {
				t.Fatal("concurrent claim replaced the emitted capability", err)
			}
		}
	}
	if count != 1 {
		t.Fatalf("concurrent hooks emitted %d recovery contexts", count)
	}
}

func TestCursorRecoveryDoesNotTrustSocketWithoutLifetimeLock(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	dir, err := os.MkdirTemp("/tmp", "ab-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "inbox.sock"), Net: "unix"})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	endpoint, err := InspectEndpoint(listener.Addr().String())
	state := cursorState(to)
	state.Wake = endpoint
	setCursorState(&to, state)
	if err != nil {
		t.Fatal(err)
	}
	to.Receive = ReceiveCapabilities{IdleWake: true, Delivery: "native_background_shell_output"}
	if err := store.locked(func() error { return store.saveSession(to) }); err != nil {
		t.Fatal(err)
	}
	// The socket itself is real and bound, but no authenticated listener owns
	// its lifetime lock. Neither a missing lock nor an unlocked file is proof.
	for _, createLock := range []bool{false, true} {
		if createLock {
			if err := os.WriteFile(store.cursorListenerLockPath(to.ID), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if store.cursorListenerRunning(to) {
			t.Fatal("trusted an unrelated socket without listener lock ownership")
		}
	}
	text, err := store.cursorRecoveryContext(to.ID)
	if err != nil || text == "" {
		t.Fatal("stale listener did not trigger recovery", err)
	}
	recoveryKey(t, text)
	current, err := store.session(to.ID)
	if err != nil || cursorState(current).Wake != nil || current.Receive.IdleWake {
		t.Fatal("recovery retained a false idle-wake advertisement", err)
	}
}

func startRecoveryListener(t *testing.T, store Store, actor, key string) (func(), *wakeOutput) {
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
	store := fixtureStore(t)
	from, fromKey := register(t, store, "codex")
	to, oldKey := register(t, store, "cursor")
	text, err := store.cursorRecoveryContext(to.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := recoveryKey(t, text)
	stop, _ := startRecoveryListener(t, store, to.ID, key)
	if text, err := store.cursorRecoveryContext(to.ID); err != nil || text != "" {
		t.Fatal("attempted recovery while the native listener held its live lock", err)
	}
	if _, err := store.authenticate(to.ID, key); err != nil {
		t.Fatal("healthy listener check changed its capability", err)
	}
	stop()
	text, err = store.cursorRecoveryContext(to.ID)
	if err != nil {
		t.Fatal(err)
	}
	key = recoveryKey(t, text)
	_, output := startRecoveryListener(t, store, to.ID, key)
	service := Service{Store: store}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: fromKey, To: to.ID, MessageID: newID(), Body: "after restart"})
	if err != nil || msg.Status != "wake_hint_written" || msg.TargetGeneration != to.Generation {
		t.Fatalf("recovered listener did not receive the original conversation's message: %+v %v", msg, err)
	}
	if !strings.Contains(output.text(), "ZALKI_WAKE "+msg.ID) {
		t.Fatal("replacement listener did not emit a wake hint")
	}
	if _, err := store.Ack(to.ID, key, msg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.authenticate(to.ID, oldKey); err == nil {
		t.Fatal("pre-recovery capability remained valid")
	}
}

type failedRecoveryOutput struct{}

func (failedRecoveryOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCursorRecoveryOutputFailureDoesNotLoop(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	text, err := store.cursorRecoveryContext(to.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.ListenCursor(context.Background(), to.ID, recoveryKey(t, text), failedRecoveryOutput{})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected native output failure, got %v", err)
	}
	if text, err := store.cursorRecoveryContext(to.ID); err != nil || text != "" {
		t.Fatal("failed task output triggered an automatic setup loop", err)
	}
	current, err := store.session(to.ID)
	if err != nil || cursorState(current).Wake != nil || current.Receive.IdleWake {
		t.Fatal("failed task still advertised a live listener", err)
	}
}
