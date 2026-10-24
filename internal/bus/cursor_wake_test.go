package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type wakeOutput struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
}

func (w *wakeOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes.HasPrefix(p, []byte("ZALKI_LISTENER_READY ")) {
		close(w.ready)
	}
	return w.buf.Write(p)
}

func (w *wakeOutput) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestCursorWakeListenerAndDelivery(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, toKey := register(t, store, "cursor")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := &wakeOutput{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- store.ListenCursor(ctx, to.ID, toKey, out) }()
	select {
	case err := <-done:
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
		}
		t.Fatalf("listener failed to start: %v", err)
	case <-ctx.Done():
		t.Fatal("listener startup timed out")
	case <-out.ready:
	}
	if err := store.ListenCursor(ctx, to.ID, toKey, &bytes.Buffer{}); err == nil {
		t.Fatal("allowed duplicate listeners for one conversation")
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == to.ID && (!session.Receive.IdleWake || len(session.AdapterState) != 0) {
			t.Fatal("armed listener did not advertise its receive capability")
		}
	}
	service := Service{Store: store}
	firstID := newID()
	for i := range 2 {
		id := firstID
		if i > 0 {
			id = newID()
		}
		msg, err := service.Send(ctx, SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: id, Body: "fixture peer body"})
		if err != nil || msg.Status != "wake_hint_written" || msg.AcknowledgedAt != "" {
			t.Fatalf("wake hint counted as model acknowledgement or failed: %+v %v", msg, err)
		}
		context, err := store.EmitPending(to.ID)
		if err != nil || !strings.Contains(context, "fixture peer body") {
			t.Fatalf("wake hint lost deferred context: %q %v", context, err)
		}
		if _, err := store.Ack(to.ID, toKey, id); err != nil {
			t.Fatal(err)
		}
		msg, err = store.Finish(id, Receipt{Status: "wake_hint_written", At: timestamp()})
		if err != nil || msg.Status != "hook_context_emitted" || msg.AcknowledgedAt == "" {
			t.Fatalf("late receipt lost hook emission or ACK: %+v %v", msg, err)
		}
	}
	if _, err := service.Send(ctx, SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: firstID, Body: "fixture peer body"}); err != nil {
		t.Fatal(err)
	}
	if text := out.text(); strings.Count(text, "ZALKI_WAKE ") != 2 || strings.Contains(text, "fixture peer body") || strings.Contains(text, toKey) {
		t.Fatalf("duplicate wake, peer text, or credential in output: %q", text)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := store.session(to.ID)
	if err != nil || current.Receive.IdleWake || cursorState(current).Wake != nil {
		t.Fatalf("listener shutdown left wake capability enabled: %+v %v", current, err)
	}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "deferred after shutdown"})
	if err != nil || msg.Status != "pending_next_activity" {
		t.Fatalf("shutdown did not retain deferred delivery: %+v %v", msg, err)
	}
}

func TestCursorWakeRejectsMisboundHints(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, toKey := register(t, store, "cursor")
	endpoint := &Endpoint{Path: "/fixture/socket"}
	state := cursorState(to)
	state.Wake = endpoint
	setCursorState(&to, state)
	if err := store.locked(func() error { return store.saveSession(to) }); err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(msg.ID, Receipt{Status: "pending_next_activity"}); err != nil {
		t.Fatal(err)
	}
	for _, hint := range []cursorWakeHint{
		{MessageID: msg.ID, Generation: "old-generation"},
		{MessageID: newID(), Generation: to.Generation},
		{MessageID: "\nZALKI_WAKE injected", Generation: to.Generation},
	} {
		client, server := net.Pipe()
		var out bytes.Buffer
		done := make(chan error, 1)
		go func() {
			defer server.Close()
			done <- store.consumeCursorHint(server, to.ID, to.Generation, endpoint, &out)
		}()
		if err := json.NewEncoder(client).Encode(hint); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil || out.Len() != 0 {
			t.Fatalf("misbound hint produced model output: %q %v", out.String(), err)
		}
		client.Close()
	}
	if err := store.ListenCursor(context.Background(), to.ID, "wrong capability", &bytes.Buffer{}); err == nil {
		t.Fatal("listener accepted a different session capability")
	}
	if err := store.ListenCursor(context.Background(), from.ID, key, &bytes.Buffer{}); err == nil {
		t.Fatal("listener accepted a Codex identity")
	}
	if _, err := store.Ack(to.ID, toKey, msg.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCursorPendingDoesNotReplayPreviousGeneration(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, _ := register(t, store, "cursor")
	service := Service{Store: store}
	if _, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "previous registration"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register("cursor", strings.TrimPrefix(to.ID, "cursor:"), to.CWD, nil); err != nil {
		t.Fatal(err)
	}
	if text, err := store.EmitPending(to.ID); err != nil || text != "" {
		t.Fatalf("replayed a message into a replacement registration: %q %v", text, err)
	}
}
