package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// Retirement outcome only: a hook on a retired session may be rejected.
func cursorHookAllowingError(ctx context.Context, service *bus.Service, actor, event, status string) {
	input, _ := json.Marshal(map[string]any{"conversation_id": strings.TrimPrefix(actor, "cursor:"),
		"hook_event_name": event, "status": status})
	_ = service.Hook(ctx, "cursor", bytes.NewReader(input), &bytes.Buffer{})
}

func TestCursorOnlyAnActualPromptReactivatesARetiredConversation(t *testing.T) {
	store := adapterTestStore(t)
	to, key := registerAdapterSession(t, store, "cursor")
	service := bus.Service{Store: store}
	cursorHook(t, &service, to.ID, "sessionEnd", "", 0)
	for _, event := range []struct{ name, status string }{{"postToolUse", ""}, {"stop", "completed"}} {
		cursorHookAllowingError(t.Context(), &service, to.ID, event.name, event.status)
		if current := adapterTestSession(t, store, to.ID); !current.Retired || current.State != model.StateRetired {
			t.Fatalf("late %s hook reactivated a retired conversation", event.name)
		}
	}
	cursorHook(t, &service, to.ID, "beforeSubmitPrompt", "", 0)
	current := adapterTestSession(t, store, to.ID)
	if current.Retired || current.RetiredReason != "" || current.State != model.StateBusy || current.Generation != to.Generation {
		t.Fatalf("prompt did not resume the same registration: %+v", current)
	}
	if _, err := store.Messages(to.ID, key); err != nil {
		t.Fatal("resumed conversation lost its capability", err)
	}

	unknown := "cursor:" + newTestID()
	cursorHook(t, &service, unknown, "beforeSubmitPrompt", "", 0)
	// List hides retired and exited records; check the journal directly.
	if _, err := os.Stat(filepath.Join(store.Dir, "sessions", strings.ReplaceAll(unknown, ":", "_")+".json")); err != nil {
		t.Fatal("C01_PROMPT_REGISTERS_RESUMED_UNKNOWN_CHAT", err)
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
	if err := listenCursorWithStore(ctx, store, old.ID, key, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already has a listener") || time.Since(started) > time.Second {
		t.Fatal("a second listener was not promptly refused for lifetime lock ownership", err)
	}
	report, err := store.Diagnostics(t.Context())
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

// A crash during retirement can leave a lease on a retired record. Prune must
// drop that leftover lease, and a dry run must only report it.
func TestPruneReleasesALeaseLeftOnARetiredSession(t *testing.T) {
	store := adapterTestStore(t)
	to, _ := registerAdapterSession(t, store, "cursor")
	path := filepath.Join(store.Dir, "sessions", strings.ReplaceAll(to.ID, ":", "_")+".json")
	var record map[string]any
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &record) != nil {
		t.Fatal("unreadable session record", err)
	}
	record["state"] = "retired"
	record["retired"] = true
	record["retired_reason"] = "explicit_cleanup"
	record["listener"] = map[string]any{"id": newTestID(), "generation": to.Generation, "endpoint": map[string]any{"path": "/tmp/zalki-cursor-crashed/inbox.sock", "device": 1, "inode": 1}}
	if err := writeTestJSON(path, record); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := store.Prune(true, nil)
	if err != nil || len(preview.Sessions) != 1 || preview.Sessions[0].ID != to.ID || preview.Sessions[0].Reason != "lease_released" {
		t.Fatalf("dry-run did not report the leftover lease: %+v %v", preview, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("dry-run changed the retired lease", err)
	}
	report, err := store.Prune(false, nil)
	if err != nil || len(report.Sessions) != 1 || report.Sessions[0].Reason != "lease_released" {
		t.Fatalf("prune did not release the leftover lease: %+v %v", report, err)
	}
	current := adapterTestSession(t, store, to.ID)
	healed := sessionFile(t, store, to.ID)
	if !current.Retired || current.RetiredReason != "explicit_cleanup" || current.State != model.StateRetired ||
		current.Listener != nil || current.Schema != 1 || current.Receive.IdleWake ||
		healed["schema"] != float64(1) || healed["listener"] != nil {
		t.Fatalf("retired record kept its lease: record=%v session=%+v", healed, current)
	}
	if again, err := store.Prune(false, nil); err != nil || len(again.Sessions) != 0 {
		t.Fatalf("releasing a leftover lease was not idempotent: %+v %v", again, err)
	}
}

// Retirement must drop the listener lease. A listener that is still running is
// fenced: it stays up, and it receives no wake.
func TestRetirementReleasesTheListenerLease(t *testing.T) {
	retire := func(t *testing.T, store bus.Store, service *bus.Service, id, how string) {
		t.Helper()
		switch how {
		case "explicit_cleanup":
			report, err := store.Prune(false, []string{id})
			if err != nil || len(report.Sessions) != 1 || report.Sessions[0].Reason != "explicit_cleanup" {
				t.Fatalf("explicit prune failed: %+v %v", report, err)
			}
		case "owner_exited":
			markOwnerExited(t, store, id)
			report, err := store.Prune(false, nil)
			if err != nil || len(report.Sessions) != 1 || report.Sessions[0].ID != id || report.Sessions[0].Reason != "owner_exited" {
				t.Fatalf("owner prune failed: %+v %v", report, err)
			}
		case "session_end":
			cursorHook(t, service, id, "sessionEnd", "", 0)
		default:
			t.Fatalf("unknown retirement %s", how)
		}
	}
	for _, how := range []string{"explicit_cleanup", "owner_exited", "session_end"} {
		t.Run(how, func(t *testing.T) {
			store := adapterTestStore(t)
			from, fromKey := registerAdapterSession(t, store, "codex")
			to, key := registerAdapterSession(t, store, "cursor")
			stop, output := startRecoveryListener(t, store, to.ID, key)
			leased := adapterTestSession(t, store, to.ID)
			if leased.Listener == nil || leased.Schema != 1 || !leased.Receive.IdleWake {
				t.Fatal("listener was not leased")
			}
			endpoint := leased.Listener.Endpoint.Path
			// Accept starts only after Confirm's save. Waiting for that avoids a
			// confirmation write racing with retirement.
			waitUntilListening(t, endpoint)
			service := &bus.Service{Store: store}
			retire(t, store, service, to.ID, how)

			current := adapterTestSession(t, store, to.ID)
			record := sessionFile(t, store, to.ID)
			receive, _ := record["receive"].(map[string]any)
			if !current.Retired || current.Listener != nil || current.Receive.IdleWake || current.Schema != 1 ||
				record["schema"] != float64(1) || record["listener"] != nil || receive["idle_wake"] != false {
				t.Fatalf("retirement left a listener lease: record=%v session=%+v", record, current)
			}
			if _, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: fromKey, To: to.ID, MessageID: newTestID(), Body: "after retire"}); err == nil {
				t.Fatal("sent to a retired session")
			}
			conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "unix", endpoint)
			if err != nil {
				t.Fatal("retired listener was not still accepting", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			hint := map[string]string{"message_id": newTestID(), "generation": to.Generation}
			if err := json.NewEncoder(conn).Encode(hint); err != nil {
				t.Fatal(err)
			}
			var response map[string]string
			if err := json.NewDecoder(conn).Decode(&response); err == nil {
				t.Fatal("fenced listener accepted a wake hint", response)
			}
			if strings.Contains(output.text(), "ZALKI_WAKE") {
				t.Fatal("retired listener emitted a wake")
			}
			stop()
			after := sessionFile(t, store, to.ID)
			if after["schema"] != float64(1) || after["listener"] != nil || after["retired"] != true {
				t.Fatalf("listener shutdown restored the lease: %v", after)
			}
		})
	}
}

// waitUntilListening returns after the listener has accepted and closed one
// connection. Accept begins only after Confirm's save, and a queued dial can
// succeed before that save.
func waitUntilListening(t *testing.T, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := (&net.Dialer{Timeout: 50 * time.Millisecond}).DialContext(t.Context(), "unix", endpoint)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, writeErr := conn.Write([]byte("{}\n"))
			buf := make([]byte, 1)
			_, readErr := conn.Read(buf)
			_ = conn.Close()
			if writeErr == nil && readErr != nil {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("listener did not accept after confirmation", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func markOwnerExited(t *testing.T, store bus.Store, id string) {
	t.Helper()
	path := filepath.Join(store.Dir, "sessions", strings.ReplaceAll(id, ":", "_")+".json")
	var session model.Session
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &session) != nil {
		t.Fatal("unreadable session record", err)
	}
	session.Owner = &model.ProcessRef{PID: os.Getpid(), Start: "previous-process-at-this-pid", Role: "client"}
	if err := writeTestJSON(path, session); err != nil {
		t.Fatal(err)
	}
}

func sessionFile(t *testing.T, store bus.Store, id string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.Dir, "sessions", strings.ReplaceAll(id, ":", "_")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
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
	err := listenCursorWithStore(context.Background(), store, to.ID, key, failedListenerOutput{})
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
