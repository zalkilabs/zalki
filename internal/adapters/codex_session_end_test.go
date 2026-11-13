package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	codexclient "github.com/zalkilabs/zalki/internal/adapters/codex"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func codexEndFixture(t *testing.T, allow bool) (bus.Store, string, string) {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		dir := filepath.Join(root, strings.ToLower(key))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	args, calls := filepath.Join(root, "argv"), filepath.Join(root, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ZAL34_TEST_ARGS\"\nprintf q >> \"$ZAL34_TEST_CALLS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("ZAL34_TEST_ARGS", args)
	t.Setenv("ZAL34_TEST_CALLS", calls)
	store, err := bus.NewStore(filepath.Join(root, "state"), adapters.BuiltinWithCodex(allow, filepath.Join(root, "config.json")), nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, args, calls
}

func sessionEndInput(target model.Session) []byte {
	event := "SessionEnd"
	if target.Kind == "cursor" {
		event = "sessionEnd"
	}
	raw, _ := json.Marshal(map[string]string{"session_id": target.NativeID, "conversation_id": target.NativeID, "cwd": target.CWD, "hook_event_name": event, "reason": "other"})
	return raw
}

func endFixtureSession(ctx context.Context, service *bus.Service, target model.Session) error {
	return service.Hook(ctx, target.Kind, bytes.NewReader(sessionEndInput(target)), io.Discard)
}

func assertCodexEndReady(t *testing.T, store bus.Store, target model.Session) {
	t.Helper()
	current := adapterTestSession(t, store, target.ID)
	if current.Retired || current.State != model.StateReady || current.ID != target.ID || current.NativeID != target.NativeID || current.Generation != target.Generation {
		t.Fatal("ZAL34_CODEX_END_RETIRED_OR_CHANGED_IDENTITY")
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range listed {
		if session.ID == target.ID {
			if session.Liveness != model.LivenessUnknown {
				t.Fatal("ZAL34_CODEX_END_INVENTED_LIVENESS")
			}
			return
		}
	}
	t.Fatal("ZAL34_CODEX_END_NOT_LISTED")
}

func assertFakeCodexQueue(t *testing.T, args string, msg model.Message, target model.Session) {
	t.Helper()
	raw, err := os.ReadFile(args)
	if err != nil {
		t.Fatal("ZAL34_CODEX_END_DID_NOT_QUEUE", err)
	}
	parts := strings.SplitN(strings.TrimSuffix(string(raw), "\n"), "\n", 5)
	if len(parts) != 5 || parts[0] != "queue" || parts[1] != "--thread" || parts[2] != target.NativeID || parts[3] != "--message" || !model.MatchesEnvelope(msg, parts[4]) {
		t.Fatal("ZAL34_CODEX_END_QUEUED_WRONG_TARGET_OR_ENVELOPE")
	}
}

func TestCodexSessionEndKeepsListedAndQueues(t *testing.T) {
	store, args, _ := codexEndFixture(t, true)
	sender, key := registerAdapterSession(t, store, "cursor")
	target, targetKey := registerAdapterSession(t, store, "codex")
	if err := store.Update(target.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	if err := endFixtureSession(t.Context(), &service, target); err != nil {
		t.Fatal(err)
	}
	assertCodexEndReady(t, store, target)
	if _, err := store.Messages(target.ID, targetKey); err != nil {
		t.Fatal("ZAL34_CODEX_END_CHANGED_CAPABILITY")
	}
	msg, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newTestID(), Body: "bounded queued end fixture"})
	if err != nil || msg.Status != model.StatusSubmitted {
		t.Fatal("ZAL34_CODEX_END_DID_NOT_QUEUE", err)
	}
	assertFakeCodexQueue(t, args, msg, target)
}

func TestCodexSessionEndPreservesQueueOptIn(t *testing.T) {
	store, _, calls := codexEndFixture(t, false)
	sender, key := registerAdapterSession(t, store, "cursor")
	target, _ := registerAdapterSession(t, store, "codex")
	service := bus.Service{Store: store}
	if err := endFixtureSession(t.Context(), &service, target); err != nil {
		t.Fatal(err)
	}
	assertCodexEndReady(t, store, target)
	msg, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newTestID(), Body: "queue disabled fixture"})
	if err != nil || msg.Status != model.StatusNotSent {
		t.Fatal("ZAL34_CODEX_END_ENABLED_QUEUE", err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("ZAL34_CODEX_END_ENABLED_QUEUE")
	}
}

func TestCodexSessionEndPreservesPendingMessages(t *testing.T) {
	for _, approval := range []bool{false, true} {
		name := "native queued"
		if approval {
			name = "approval held"
		}
		t.Run(name, func(t *testing.T) { checkCodexPendingEnd(t, approval) })
	}
}

func checkCodexPendingEnd(t *testing.T, approval bool) {
	t.Helper()
	store, args, calls := codexEndFixture(t, true)
	sender, key := registerAdapterSession(t, store, "cursor")
	target, targetKey := registerAdapterSession(t, store, "codex")
	if approval {
		if err := store.Update(target.ID, model.StateApproval, false); err != nil {
			t.Fatal(err)
		}
	}
	service := bus.Service{Store: store}
	msg, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newTestID(), Body: "pending end fixture"})
	if err != nil {
		t.Fatal(err)
	}
	expected := model.StatusSubmitted
	if approval {
		expected = model.StatusApprovalBlocked
	}
	if msg.Status != expected {
		t.Fatal("pending fixture did not reach its intended state")
	}
	if err := endFixtureSession(t.Context(), &service, target); err != nil {
		t.Fatal(err)
	}
	saved := codexPendingAfterEnd(t, store, target, msg.ID)
	assertFakeCodexQueue(t, args, saved, target)
	if err := endFixtureSession(t.Context(), &service, target); err != nil {
		t.Fatal(err)
	}
	attempts, err := os.ReadFile(calls)
	if err != nil || string(attempts) != "q" {
		t.Fatal("ZAL34_CODEX_END_REPLAYED_PENDING_MESSAGE")
	}
	if _, err := store.Ack(target.ID, targetKey, msg.ID); err != nil {
		t.Fatal("ZAL34_CODEX_END_PENDING_CANNOT_ACK")
	}
}

func TestOtherSessionEndStillRetires(t *testing.T) {
	for _, kind := range []string{"claude", "cursor"} {
		t.Run(kind, func(t *testing.T) {
			store, _, _ := codexEndFixture(t, true)
			target, _ := registerAdapterSession(t, store, kind)
			service := bus.Service{Store: store}
			if err := endFixtureSession(t.Context(), &service, target); err != nil {
				t.Fatal(err)
			}
			current := adapterTestSession(t, store, target.ID)
			if !current.Retired || current.State != model.StateRetired || current.RetiredReason != "session_end" {
				t.Fatal("ZAL34_OTHER_END_DID_NOT_RETIRE")
			}
		})
	}
}

func TestCodexSessionStartAfterEndKeepsActorID(t *testing.T) {
	store, _, _ := codexEndFixture(t, true)
	target, oldKey := registerAdapterSession(t, store, "codex")
	service := bus.Service{Store: store}
	if err := endFixtureSession(t.Context(), &service, target); err != nil {
		t.Fatal(err)
	}
	current, newKey := codexStartHook(t, &service, target, "resume")
	if current.ID != target.ID || current.NativeID != target.NativeID || current.Retired {
		t.Fatal("ZAL34_CODEX_RESTART_CHANGED_ACTOR_ID")
	}
	if current.Generation == target.Generation || newKey == oldKey {
		t.Fatal("ZAL34_CODEX_RESTART_LOST_REGISTRATION_ROTATION")
	}
}

func TestCodexSessionEndRestartBurstKeepsAllSessions(t *testing.T) {
	store, _, _ := codexEndFixture(t, true)
	service := bus.Service{Store: store}
	targets := make([]model.Session, 12)
	for i := range targets {
		targets[i], _ = registerAdapterSession(t, store, "codex")
	}
	start := make(chan struct{})
	results := make(chan error, len(targets))
	ctx := t.Context()
	for _, target := range targets {
		go func() { <-start; results <- endFixtureSession(ctx, &service, target) }()
	}
	close(start)
	for range targets {
		if err := <-results; err != nil {
			t.Error("ZAL34_CODEX_RESTART_BURST_FAILED", err)
		}
	}
	for _, target := range targets {
		current := adapterTestSession(t, store, target.ID)
		if current.Retired || current.State != model.StateReady || current.Generation != target.Generation {
			t.Fatal("ZAL34_CODEX_RESTART_BURST_RETIRED_SESSION")
		}
	}
	listed, err := store.List()
	if err != nil || len(listed) != len(targets) {
		t.Fatal("ZAL34_CODEX_RESTART_BURST_LOST_SESSION")
	}
}

func TestPruneCodexNeedsEvidenceOrExplicitID(t *testing.T) {
	store, _, _ := codexEndFixture(t, true)
	target, _ := registerAdapterSession(t, store, "codex")
	service := bus.Service{Store: store}
	if err := endFixtureSession(t.Context(), &service, target); err != nil {
		t.Fatal(err)
	}
	assertPruneKeepsUnknownCodex(t, store, target)
	report, err := store.Prune(true, []string{target.ID})
	if err != nil || len(report.Sessions) != 1 || report.Sessions[0].Reason != "explicit_cleanup" {
		t.Fatal("ZAL34_PRUNE_EXACT_ID_LOST", err)
	}
	if _, err := store.Prune(false, []string{target.ID}); err != nil {
		t.Fatal(err)
	}
	if err := endFixtureSession(t.Context(), &service, target); err != nil {
		t.Fatal(err)
	}
	current := adapterTestSession(t, store, target.ID)
	if !current.Retired || current.State != model.StateRetired || current.RetiredReason != "explicit_cleanup" {
		t.Fatal("ZAL34_CODEX_LATE_END_REVIVED_EXPLICIT_RETIREMENT")
	}
}

func codexPendingAfterEnd(t *testing.T, store bus.Store, target model.Session, messageID string) model.Message {
	t.Helper()
	saved := adapterTestMessage(t, store, messageID)
	current := adapterTestSession(t, store, target.ID)
	if saved.Status != model.StatusSubmitted || saved.AcknowledgedAt != "" || saved.TargetGeneration != target.Generation || current.Retired || current.ApprovalHeld != 0 {
		t.Fatal("ZAL34_CODEX_END_LOST_PENDING_MESSAGE")
	}
	return saved
}

func assertPruneKeepsUnknownCodex(t *testing.T, store bus.Store, target model.Session) {
	t.Helper()
	for _, dry := range []bool{true, false} {
		report, err := store.Prune(dry, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range report.Sessions {
			if item.ID == target.ID {
				t.Fatal("ZAL34_PRUNE_RETIRED_UNKNOWN_CODEX")
			}
		}
		current := adapterTestSession(t, store, target.ID)
		if current.Retired || current.Liveness == model.LivenessExited {
			t.Fatal("ZAL34_PRUNE_RETIRED_UNKNOWN_CODEX")
		}
	}
}

// Observation is test-only: the real adapter still receives the actual bound
// core host. The callback runs before Service.Hook releases held messages.
type observedCodexEnd struct {
	codexclient.Adapter
	after func()
}

func (a observedCodexEnd) HandleHook(host agent.HookHost, event agent.HookInput, id string, out io.Writer) (bool, error) {
	handled, err := a.Adapter.HandleHook(host, event, id, out)
	if event.Event == "SessionEnd" && err == nil {
		a.after()
	}
	return handled, err
}

func observeCodexEndHandler(t *testing.T, store *bus.Store, after func()) {
	t.Helper()
	kinds := store.Registry.Kinds()
	items := make([]agent.Adapter, 0, len(kinds))
	for _, kind := range kinds {
		adapter, err := store.Registry.Lookup(kind)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "codex" {
			concrete, ok := adapter.(codexclient.Adapter)
			if !ok {
				t.Fatal("fixture lost concrete Codex adapter")
			}
			adapter = observedCodexEnd{Adapter: concrete, after: after}
		}
		items = append(items, adapter)
	}
	registry, err := agent.NewRegistry(items...)
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
}

func TestCodexSessionEndLeavesHeldUntilNormalRelease(t *testing.T) {
	for _, allow := range []bool{false, true} {
		name := "fallback off"
		if allow {
			name = "fallback on"
		}
		t.Run(name, func(t *testing.T) { checkCodexEndReleaseBoundary(t, allow) })
	}
}

func checkCodexEndReleaseBoundary(t *testing.T, allow bool) {
	t.Helper()
	store, args, calls := codexEndFixture(t, allow)
	sender, key := registerAdapterSession(t, store, "cursor")
	target, _ := registerAdapterSession(t, store, "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	msg, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newTestID(), Body: "release boundary fixture"})
	if err != nil || msg.Status != model.StatusApprovalBlocked {
		t.Fatal("release boundary precondition failed", err)
	}
	observed := false
	observeCodexEndHandler(t, &store, func() { observed = true; assertHeldBeforeEndRelease(t, store, target, msg.ID, calls) })
	service.Store = store
	if err := endFixtureSession(t.Context(), &service, target); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("ZAL34_END_RELEASE_BOUNDARY_NOT_OBSERVED")
	}
	assertCodexEndReady(t, store, target)
	assertEndReleaseOutcome(t, &service, allow, sender, key, target, msg.ID, args, calls)
}

func assertHeldBeforeEndRelease(t *testing.T, store bus.Store, target model.Session, messageID, calls string) {
	t.Helper()
	saved := adapterTestMessage(t, store, messageID)
	current := adapterTestSession(t, store, target.ID)
	if saved.Status != model.StatusApprovalBlocked || saved.AcknowledgedAt != "" || saved.TargetGeneration != target.Generation || current.ApprovalHeld != 1 || current.Retired || current.State != model.StateReady {
		t.Fatal("ZAL34_END_SETTLED_HELD_BEFORE_RELEASE")
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("ZAL34_END_INVOKED_TRANSPORT_BEFORE_RELEASE")
	}
}

func assertEndReleaseOutcome(t *testing.T, service *bus.Service, allow bool, sender model.Session, key string, target model.Session, messageID, args, calls string) {
	t.Helper()
	saved := adapterTestMessage(t, service.Store, messageID)
	current := adapterTestSession(t, service.Store, target.ID)
	if current.ApprovalHeld != 0 || current.Retired {
		t.Fatal("ZAL34_END_NORMAL_RELEASE_NOT_FINISHED")
	}
	if allow {
		if saved.Status != model.StatusSubmitted || saved.AcknowledgedAt != "" {
			t.Fatal("ZAL34_END_OWNER_QUEUE_NOT_SUBMITTED")
		}
		assertFakeCodexQueue(t, args, saved, target)
		attempts, err := os.ReadFile(calls)
		if err != nil || string(attempts) != "q" {
			t.Fatal("ZAL34_END_OWNER_QUEUE_DISPATCH_COUNT")
		}
		return
	}
	assertVisibleEndQueueRefusal(t, saved, target)
	retry, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newTestID(), Body: "sender can retry fixture"})
	if err != nil || retry.ID == saved.ID {
		t.Fatal("ZAL34_END_REFUSAL_PREVENTED_SENDER_RETRY", err)
	}
	assertVisibleEndQueueRefusal(t, retry, target)
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("ZAL34_END_REFUSAL_BYPASSED_QUEUE_OPT_IN")
	}
}

func assertVisibleEndQueueRefusal(t *testing.T, msg model.Message, target model.Session) {
	t.Helper()
	if msg.Status != model.StatusNotSent || msg.Receipt == nil || !strings.Contains(msg.Receipt.Error, "codex.allow_queue_fallback=true") || strings.Contains(strings.ToLower(msg.Receipt.Error), "retired") || msg.TargetGeneration != target.Generation {
		t.Fatal("ZAL34_END_REFUSAL_NOT_HONEST_TRANSPORT_REASON")
	}
}
