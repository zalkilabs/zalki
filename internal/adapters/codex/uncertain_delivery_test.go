package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

type uncertainNativeFixture struct {
	service     bus.Service
	request     bus.SendRequest
	target      model.Session
	calls       <-chan observedCodex
	queue       string
	connections *atomic.Int32
}

func newUncertainNativeFixture(t *testing.T, mode string, allow bool) uncertainNativeFixture {
	t.Helper()
	root, listener := listenCodexControl(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	registry, err := agent.NewRegistry(New(allow, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	from, key, err := store.Register("codex", newTestID(), "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := store.Register("codex", newTestID(), "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan observedCodex, 32)
	connections := &atomic.Int32{}
	spec := fakeCodex{Home: root, Thread: to.NativeID, Mode: mode}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { connections.Add(1); calls <- handleFakeCodex(w, r, spec) })}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	return uncertainNativeFixture{service: bus.Service{Store: store}, request: bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newTestID(), Body: "private uncertainty fixture"}, target: to, calls: calls, queue: queueSpy(t), connections: connections}
}

func TestCodexUncertainDeliveryNeverReplays(t *testing.T) {
	for _, mode := range []string{"dropped", "hang", "empty-turn"} {
		for _, allow := range []bool{false, true} {
			name := mode + map[bool]string{false: "/queue-off", true: "/queue-on"}[allow]
			t.Run(name, func(t *testing.T) { checkUncertainNativeDelivery(t, mode, allow) })
		}
	}
}

func uncertainNativeTimeout(mode string) time.Duration {
	if mode == "hang" {
		return 400 * time.Millisecond
	}
	return 3 * time.Second
}

func checkUncertainNativeDelivery(t *testing.T, mode string, allow bool) {
	t.Helper()
	f := newUncertainNativeFixture(t, mode, allow)
	ctx, cancel := context.WithTimeout(t.Context(), uncertainNativeTimeout(mode))
	defer cancel()
	msg, err := f.service.Send(ctx, f.request)
	if queueMarked(t, f.queue) {
		t.Fatal("possibly accepted message was submitted again")
	}
	want := model.StatusUnknownWrite
	if mode == "hang" {
		want = model.StatusUnknownTimeout
	}
	if err != nil || msg.Status != want || msg.ID != f.request.MessageID || msg.AcknowledgedAt != "" || msg.Held {
		t.Fatal("native uncertainty was silently acknowledged or confirmed", msg.Status, err)
	}
	select {
	case seen := <-f.calls:
		submits := 0
		for _, method := range seen.methods {
			if method == "turn/start" {
				submits++
			}
		}
		if submits != 1 || seen.turn == nil || queueMarked(t, f.queue) {
			t.Fatal("possibly accepted message was submitted again", submits)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("uncertain native exchange did not finish")
	}
	checkUncertainNativeLifecycle(t, &f, want)
}

func checkUncertainNativeLifecycle(t *testing.T, f *uncertainNativeFixture, want model.DeliveryStatus) {
	t.Helper()
	for _, event := range []string{"Stop", "SessionEnd", "SessionStart", "PostToolUse"} {
		raw, _ := json.Marshal(map[string]string{"session_id": f.target.NativeID, "hook_event_name": event, "source": "compact", "cwd": "/fixture"})
		if err := f.service.Hook(t.Context(), "codex", bytes.NewReader(raw), io.Discard); err != nil {
			t.Fatal("uncertain lifecycle failed", event, err)
		}
	}
	msg, err := f.service.Send(t.Context(), f.request)
	if err != nil || msg.Status != want {
		t.Fatal("same-ID lookup retried native delivery", msg.Status, err)
	}
	if f.connections.Load() != 1 {
		t.Fatal("same-ID lookup retried native delivery")
	}
	if queueMarked(t, f.queue) {
		t.Fatal("possibly accepted message was submitted again")
	}
	changed := f.request
	changed.Body = "changed"
	if _, err := f.service.Send(t.Context(), changed); err == nil {
		t.Fatal("same-ID lookup accepted changed payload")
	}
	history, err := f.service.Store.QueryMessages(bus.MessageQuery{Actor: f.request.Actor, Capability: f.request.Capability, MessageID: msg.ID, History: true})
	if err != nil || len(history) != 1 || history[0].Status != want || history[0].AcknowledgedAt != "" {
		t.Fatal("compaction lost original uncertain record", err)
	}
}
