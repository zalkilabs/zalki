package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestCursorReceiveIsDeferredAndDoesNotRestartInterruptedTurn(t *testing.T) {
	store := adapterTestStore(t)
	sender, key := registerAdapterSession(t, store, "codex")
	target, targetKey := registerAdapterSession(t, store, "cursor")
	if target.Receive.IdleWake {
		t.Fatal("Cursor incorrectly advertises idle wake")
	}
	service := bus.Service{Store: store, Deliver: func(context.Context, bus.Message, bus.Session) bus.Receipt {
		t.Fatal("used unsupported Cursor transport")
		return bus.Receipt{}
	}}
	id := newTestID()
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "cursor body"})
	if err != nil || msg.Status != "pending_next_activity" {
		t.Fatalf("%+v %v", msg, err)
	}
	callHook := func(event, status string) *bytes.Buffer {
		input, _ := json.Marshal(map[string]any{"conversation_id": strings.TrimPrefix(target.ID, "cursor:"), "hook_event_name": event, "status": status})
		out := &bytes.Buffer{}
		if err := service.Hook("cursor", bytes.NewReader(input), out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := callHook("stop", "aborted"); out.Len() != 0 {
		t.Fatal("auto-continued an interrupted session")
	}
	if msg := adapterTestMessage(t, store, id); msg.Status != "pending_next_activity" {
		t.Fatalf("%+v %v", msg, err)
	}
	out := callHook("postToolUse", "")
	if !strings.Contains(out.String(), "additional_context") || !strings.Contains(out.String(), "cursor body") {
		t.Fatalf("missing Cursor context: %s", out)
	}
	msg = adapterTestMessage(t, store, id)
	if msg.Status != "hook_context_emitted" || msg.AcknowledgedAt != "" {
		t.Fatalf("context emission incorrectly counted as ACK: %+v %v", msg, err)
	}
	if out := callHook("postToolUse", ""); out.Len() != 0 {
		t.Fatal("context emitted twice")
	}
	if _, err := store.Ack(target.ID, targetKey, id); err != nil {
		t.Fatal(err)
	}
}

func TestCursorStartupUsesNativeHookSchema(t *testing.T) {
	store := adapterTestStore(t)
	service := bus.Service{Store: store}
	id := newTestID()
	input, _ := json.Marshal(map[string]any{"session_id": id, "workspace_roots": []string{"/workspace"}, "hook_event_name": "sessionStart"})
	var out bytes.Buffer
	if err := service.Hook("cursor", bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["additional_context"] == nil || value["hookSpecificOutput"] != nil {
		t.Fatalf("wrong Cursor hook output: %s", out.String())
	}
	env := value["env"].(map[string]any)
	sessions, err := store.List()
	if err != nil || len(sessions) != 1 || sessions[0].ID != "cursor:"+id {
		t.Fatalf("%+v %v", sessions, err)
	}
	if _, err := store.Messages(sessions[0].ID, env["ZALKI_CAPABILITY"].(string)); err != nil {
		t.Fatal(err)
	}
}

func TestCursorDoctorSuggestsArmingAnUnarmedListener(t *testing.T) {
	store := adapterTestStore(t)
	target, _ := registerAdapterSession(t, store, "cursor")
	report, err := store.Diagnostics()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range report.Sessions {
		if d.ID != target.ID {
			continue
		}
		if d.ListenerHealth != "not_armed" || !strings.Contains(d.RecoveryHint, "cursor-listen") {
			t.Fatalf("unarmed Cursor listener has no arming step: %+v", d)
		}
		return
	}
	t.Fatal("Cursor session missing from diagnostics")
}

// A listener started by a build before leases holds the lifetime lock without a
// lease. Doctor reports it and says to replace it, rather than "not armed".
func TestCursorDoctorReportsAPreLeaseListener(t *testing.T) {
	store := adapterTestStore(t)
	target, _ := registerAdapterSession(t, store, "cursor")
	lock := filepath.Join(store.Dir, "cursor-listener-"+strings.TrimPrefix(target.ID, "cursor:")+".lock")
	fd, err := syscall.Open(lock, syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	report, err := store.Diagnostics()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range report.Sessions {
		if d.ID == target.ID {
			if d.ListenerHealth != "legacy_running" || !strings.Contains(d.RecoveryHint, "earlier Zalki build or registration") {
				t.Fatalf("pre-lease listener was not reported: %+v", d)
			}
			return
		}
	}
	t.Fatal("Cursor session missing from diagnostics")
}
