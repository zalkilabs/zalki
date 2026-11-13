package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters/cursor"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

type pausedCursor struct {
	cursor.Adapter
	operation string
	arrived   chan struct{}
	resume    chan struct{}
	used      *atomic.Bool
}

func (a pausedCursor) HandleHook(host agent.HookHost, event agent.HookInput, actor string, output io.Writer) (bool, error) {
	gate := func(operation string) {
		if operation == a.operation && a.used.CompareAndSwap(false, true) {
			close(a.arrived)
			<-a.resume
		}
	}
	return a.Adapter.HandleHook(generationBarrierHost{LifecycleHost: host.(agent.LifecycleHost), gate: gate}, event, actor, output)
}

type generationBarrierHost struct {
	agent.LifecycleHost
	gate func(string)
}

func (h generationBarrierHost) UpdateSession(actor string, state model.SessionState, retired bool) error {
	h.gate("update")
	return h.LifecycleHost.UpdateSession(actor, state, retired)
}

func (h generationBarrierHost) ApplyLifecycle(policy func(model.SessionView) (agent.LifecycleChange, error)) error {
	h.gate("apply")
	return h.LifecycleHost.ApplyLifecycle(policy)
}

func (h generationBarrierHost) ClaimRecovery(policy func(model.SessionView) (agent.RecoveryDecision, error)) (agent.RecoveryGrant, error) {
	h.gate("claim")
	grant, err := h.LifecycleHost.ClaimRecovery(policy)
	if err == nil && grant.Capability == "" {
		h.gate("empty_claim")
	}
	return grant, err
}

func TestCursorStaleHookCannotTouchReplacementGeneration(t *testing.T) {
	for _, operation := range []string{"update", "apply", "claim", "empty_claim"} {
		for _, replacement := range []string{"sessionStart", "prune and prompt"} {
			t.Run(operation+"/"+replacement, func(t *testing.T) { assertCursorGenerationBarrier(t, operation, replacement) })
		}
	}
}

type cursorHookResult struct {
	text string
	err  error
}

func assertCursorGenerationBarrier(t *testing.T, operation, replacement string) {
	t.Helper()
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	store := adapterTestStore(t)
	barrier := pausedCursor{operation: operation, arrived: make(chan struct{}), resume: make(chan struct{}), used: &atomic.Bool{}}
	codex, _ := store.Registry.Lookup("codex")
	claude, _ := store.Registry.Lookup("claude")
	registry, err := agent.NewRegistry(codex, claude, barrier)
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
	service := bus.Service{Store: store, Deliver: func(_ context.Context, _ model.Message, target model.Session) model.Receipt {
		if target.Kind == "cursor" {
			return model.NewReceipt(model.StatusPendingActivity)
		}
		return model.NewReceipt(model.StatusSubmitted)
	}}
	from, key := registerAdapterSession(t, store, "codex")
	old, _ := registerAdapterSession(t, store, "cursor")
	if operation == "empty_claim" {
		recoveryKey(t, cursorHook(t, &service, old.ID, "postToolUse", "", 0))
	}
	if _, err := service.Watch(from.ID, key, old.ID); err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	defer release.Do(func() { close(barrier.resume) })
	result := launchPausedCursorHook(t, &service, old.ID, operation)
	select {
	case <-barrier.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("C15_HOOK_DID_NOT_REACH_BARRIER")
	}
	replacePausedCursor(t, &service, old, replacement)
	queueReplacementApproval(t, &service, from.ID, key, old.ID)
	before := cursorJournalBytes(t, store)
	release.Do(func() { close(barrier.resume) })
	assertStaleCursorResult(t, result, before, store)
}

func launchPausedCursorHook(t *testing.T, service *bus.Service, actor, operation string) <-chan cursorHookResult {
	t.Helper()
	event := "stop"
	if operation == "apply" {
		event = "beforeSubmitPrompt"
	}
	raw, err := json.Marshal(map[string]any{"conversation_id": strings.TrimPrefix(actor, "cursor:"), "hook_event_name": event, "status": "completed"})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan cursorHookResult, 1)
	go func() {
		var output bytes.Buffer
		err := service.Hook(t.Context(), "cursor", bytes.NewReader(raw), &output)
		result <- cursorHookResult{text: output.String(), err: err}
	}()
	return result
}

func replacePausedCursor(t *testing.T, service *bus.Service, old model.Session, replacement string) {
	t.Helper()
	event := "sessionStart"
	if replacement == "prune and prompt" {
		if _, err := service.Store.PruneRetired(false, []string{old.ID}, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Store.PruneRetired(false, nil, 0); err != nil {
			t.Fatal(err)
		}
		event = "beforeSubmitPrompt"
	}
	runCursorHook(t, service, map[string]any{"conversation_id": old.NativeID, "hook_event_name": event, "cwd": "/workspace"})
	if current := adapterTestSession(t, service.Store, old.ID); current.Generation == old.Generation {
		t.Fatal("C15_REPLACEMENT_FIXTURE_KEPT_GENERATION")
	}
}

func queueReplacementApproval(t *testing.T, service *bus.Service, from, key, actor string) {
	t.Helper()
	if err := service.Store.Update(actor, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(t.Context(), bus.SendRequest{Actor: from, Capability: key, To: actor, MessageID: newTestID(), Body: "G2_BODY_MUST_STAY_PRIVATE"}); err != nil {
		t.Fatal(err)
	}
	// Leave the hold queued while making it eligible for a later real hook.
	if err := service.Store.Update(actor, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
}

func assertStaleCursorResult(t *testing.T, result <-chan cursorHookResult, before map[string]string, store bus.Store) {
	t.Helper()
	select {
	case got := <-result:
		if got.err != nil || strings.Contains(got.text, "G2_BODY_MUST_STAY_PRIVATE") || strings.Contains(got.text, "private capability") || strings.Contains(got.text, "ZALKI_CAPABILITY") {
			t.Fatal("C15_STALE_HOOK_HAS_NO_KEY_OR_BODY", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("C15_STALE_HOOK_DID_NOT_FINISH")
	}
	if after := cursorJournalBytes(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("C15_COMPLETE_G2_STATE_MESSAGES_WATCHES_AND_QUEUE_UNCHANGED")
	}
}

func cursorJournalBytes(t *testing.T, store bus.Store) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(store.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
