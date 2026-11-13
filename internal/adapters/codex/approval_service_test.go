package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

type approvalServiceFixture struct {
	service          bus.Service
	sender, target   model.Session
	key, queueMarker string
	waiting          atomic.Bool
	submits          atomic.Int32
	calls            <-chan observedCodex
}

func newApprovalServiceFixture(t *testing.T, allow bool, rpc bool, before func(string)) *approvalServiceFixture {
	t.Helper()
	root, listener := listenCodexControl(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	f := &approvalServiceFixture{queueMarker: queueSpy(t)}
	f.waiting.Store(true)
	registry, err := agent.NewRegistry(New(allow, filepath.Join(root, "fixture-config.json")))
	if err != nil {
		t.Fatal(err)
	}
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.service.Store = store
	f.sender, f.key, err = store.Register("codex", newTestID(), "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.target, _, err = store.Register("codex", newTestID(), "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan observedCodex, 32)
	spec := fakeCodex{Home: root, Thread: f.target.NativeID, Mode: "idle", CurrentMode: func() string {
		if f.waiting.Load() {
			if rpc {
				return "approval-rpc"
			}
			return "approval"
		}
		return "idle"
	}, BeforeResponse: func(method string) {
		if before != nil {
			before(method)
		}
		if method == "turn/start" && !f.waiting.Load() {
			f.submits.Add(1)
		}
	}}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls <- handleFakeCodex(w, r, spec) })}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	f.calls = calls
	return f
}

func (f *approvalServiceFixture) send(t *testing.T, id string) (model.Message, error) {
	t.Helper()
	return f.service.Send(t.Context(), bus.SendRequest{Actor: f.sender.ID, Capability: f.key, To: f.target.ID, MessageID: id, Body: "isolated live approval fixture"})
}

func (f *approvalServiceFixture) hook(t *testing.T, event string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"session_id": f.target.NativeID, "hook_event_name": event})
	if err := f.service.Hook(t.Context(), "codex", bytes.NewReader(raw), io.Discard); err != nil {
		t.Fatal(event, err)
	}
}

func (f *approvalServiceFixture) exchange(t *testing.T, wantTurn bool) {
	t.Helper()
	select {
	case seen := <-f.calls:
		if len(seen.forbidden) != 0 || (seen.turn != nil) != wantTurn {
			t.Fatal("unexpected native approval exchange", seen.methods, seen.forbidden)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake native approval exchange did not finish")
	}
}

func (f *approvalServiceFixture) assertCount(t *testing.T, want int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.service.Store.Dir, "sessions", "codex_"+f.target.NativeID+".json"))
	var current model.Session
	if err != nil || json.Unmarshal(raw, &current) != nil || current.ApprovalHeld != want || current.Retired || current.Generation != f.target.Generation {
		t.Fatalf("native approval count/generation: count=%d want=%d retired=%v errors=%v", current.ApprovalHeld, want, current.Retired, err)
	}
	if queueMarked(t, f.queueMarker) {
		t.Fatal("positive live approval fell back to native queue")
	}
}

func TestCodexLiveApprovalHoldsThroughStopAndEnd(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "queue-off", true: "queue-on"}[allow], func(t *testing.T) {
			f := newApprovalServiceFixture(t, allow, false, nil)
			msg, err := f.send(t, newTestID())
			if err != nil || !msg.IsQueuedForApproval() {
				t.Fatal("live waitingOnApproval must hold", msg.Status, msg.Held, err)
			}
			f.exchange(t, false)
			f.assertCount(t, 1)
			for _, event := range []string{"Stop", "Stop", "SessionEnd"} {
				f.hook(t, event)
				f.exchange(t, false)
				f.assertCount(t, 1)
			}
			if f.submits.Load() != 0 {
				t.Fatal("still-waiting lifecycle submitted a turn")
			}
			f.waiting.Store(false)
			f.hook(t, "PostToolUse")
			f.exchange(t, true)
			f.assertCount(t, 0)
			f.hook(t, "Stop")
			if f.submits.Load() != 1 {
				t.Fatal("cleared approval must submit exactly once", f.submits.Load())
			}
		})
	}
}

func TestCodexApprovalRpcRejectionIsQueued(t *testing.T) {
	f := newApprovalServiceFixture(t, true, true, nil)
	msg, err := f.send(t, newTestID())
	if err != nil || !msg.IsQueuedForApproval() {
		t.Fatal("native RPC approval rejection must be held", msg.Status, msg.Held, err)
	}
	f.exchange(t, true)
	f.assertCount(t, 1)
	if f.submits.Load() != 0 {
		t.Fatal("rejected RPC was counted as accepted")
	}
	f.waiting.Store(false)
	f.hook(t, "Stop")
	f.exchange(t, true)
	f.assertCount(t, 0)
	if f.submits.Load() != 1 {
		t.Fatal("RPC rejection did not release exactly once", f.submits.Load())
	}
}

func TestCodexLateNativeApprovalPreservesDirectFinalRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered, resume := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	f := newApprovalServiceFixture(t, false, false, func(method string) {
		if method == "thread/read" {
			once.Do(func() {
				close(entered)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			})
		}
	})
	id := newTestID()
	type result struct {
		msg model.Message
		err error
	}
	done := make(chan result, 1)
	finished := make(chan struct{})
	t.Cleanup(func() { release(); cancel(); <-finished })
	go func() {
		defer close(finished)
		msg, err := f.service.Send(ctx, bus.SendRequest{Actor: f.sender.ID, Capability: f.key, To: f.target.ID, MessageID: id, Body: "concurrent final refusal"})
		done <- result{msg, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("native RPC was not paused", ctx.Err())
	}
	receipt := model.NewReceipt(model.StatusApprovalBlocked)
	receipt.Error = "authoritative direct final refusal"
	if _, err := f.service.Store.Finish(id, receipt); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.service.Store.Dir, "messages", id+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	release()
	got := <-done
	after, err := os.ReadFile(path)
	if got.err != nil || err != nil || got.msg.Held || !bytes.Equal(before, after) {
		t.Fatalf("late native approval must preserve final refusal: held=%v status=%s unchanged=%v errors=%v/%v", got.msg.Held, got.msg.Status, bytes.Equal(before, after), got.err, err)
	}
	f.exchange(t, false)
	f.assertCount(t, 0)
}

func TestCodexPermissionHookWithIdleLiveThreadDelivers(t *testing.T) {
	f := newApprovalServiceFixture(t, false, false, nil)
	f.waiting.Store(false)
	f.hook(t, "PermissionRequest")
	msg, err := f.send(t, newTestID())
	if err != nil || msg.Held || msg.Status != model.StatusSubmitted || f.submits.Load() != 1 {
		t.Fatalf("Codex permission hook plus idle live thread must deliver immediately: held=%v status=%s submits=%d error=%v", msg.Held, msg.Status, f.submits.Load(), err)
	}
	f.exchange(t, true)
	f.assertCount(t, 0)
}
