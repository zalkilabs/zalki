package adapters_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
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
	store := adapterTestStore(t)
	from, key := registerAdapterSession(t, store, "codex")
	to, toKey := registerAdapterSession(t, store, "cursor")
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
	service := bus.Service{Store: store}
	firstID := newTestID()
	for i := range 2 {
		id := firstID
		if i > 0 {
			id = newTestID()
		}
		msg, err := service.Send(ctx, bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: id, Body: "fixture peer body"})
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
		msg, err = store.Finish(id, bus.NewReceipt(bus.StatusWakeWritten))
		if err != nil || msg.Status != "hook_context_emitted" || msg.AcknowledgedAt == "" {
			t.Fatalf("late receipt lost hook emission or ACK: %+v %v", msg, err)
		}
	}
	if _, err := service.Send(ctx, bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: firstID, Body: "fixture peer body"}); err != nil {
		t.Fatal(err)
	}
	if text := out.text(); strings.Count(text, "ZALKI_WAKE ") != 2 || strings.Contains(text, "fixture peer body") || strings.Contains(text, toKey) {
		t.Fatalf("duplicate wake, peer text, or credential in output: %q", text)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current := adapterTestSession(t, store, to.ID)
	if current.Receive.IdleWake || len(current.AdapterState) != 0 && adapterTestCursorState(t, current).Wake != nil {
		t.Fatalf("listener shutdown left wake capability enabled: %+v %v", current, err)
	}
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newTestID(), Body: "deferred after shutdown"})
	if err != nil || msg.Status != "pending_next_activity" {
		t.Fatalf("shutdown did not retain deferred delivery: %+v %v", msg, err)
	}
}
