package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestCursorReceiveIsDeferredAndDoesNotRestartInterruptedTurn(t *testing.T) {
	store := adapterTestStore(t)
	sender, key := registerAdapterSession(t, store, "codex")
	target, targetKey := registerAdapterSession(t, store, "cursor")
	if target.Receive.IdleWake {
		t.Fatal("Cursor incorrectly advertises idle wake")
	}
	service := bus.Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		t.Fatal("used unsupported Cursor transport")
		return model.Receipt{}
	}}
	id := newTestID()
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "cursor body"})
	if err != nil || msg.Status != "pending_next_activity" {
		t.Fatalf("%+v %v", msg, err)
	}
	callHook := func(event, status string) *bytes.Buffer {
		input, _ := json.Marshal(map[string]any{"conversation_id": strings.TrimPrefix(target.ID, "cursor:"), "hook_event_name": event, "status": status})
		out := &bytes.Buffer{}
		if err := service.Hook(t.Context(), "cursor", bytes.NewReader(input), out); err != nil {
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
	if err := service.Hook(t.Context(), "cursor", bytes.NewReader(input), &out); err != nil {
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
	report, err := store.Diagnostics(t.Context())
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
	report, err := store.Diagnostics(t.Context())
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

// An IDE chat starts with session_id != conversation_id and no usable root, and
// later hooks carry only conversation_id. Start and later hooks must address one
// actor. The payloads follow Cursor's hook docs; they are not a recorded capture.
func TestCursorIDEStartAndLaterHooksShareOneActor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		roots   []string
		wantCWD string // "" means the test HOME
	}{
		{"no roots", []string{}, ""},
		{"file URI root", []string{"file:///fixture/proj%20dir"}, "/fixture/proj dir"},
		{"remote root", []string{"vscode-remote://ssh-remote+h/y"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			want := tc.wantCWD
			if want == "" {
				want = home
			}
			checkIDEChatSharesOneActor(t, tc.roots, want)
		})
	}
}

func checkIDEChatSharesOneActor(t *testing.T, roots []string, wantCWD string) {
	t.Helper()
	store := adapterTestStore(t)
	service := bus.Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		t.Fatal("used unsupported Cursor transport")
		return model.Receipt{}
	}}
	conversation, session := newTestID(), newTestID()
	runCursorHook(t, &service, map[string]any{"hook_event_name": "sessionStart", "session_id": session, "conversation_id": conversation, "workspace_roots": roots})
	sessions, err := store.List()
	if err != nil || len(sessions) != 1 || sessions[0].ID != "cursor:"+conversation || sessions[0].CWD != wantCWD {
		t.Fatalf("want one session cursor:%s with cwd %q: %+v %v", conversation, wantCWD, sessions, err)
	}
	sender, key := registerAdapterSession(t, store, "codex")
	id := newTestID()
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: sender.ID, Capability: key, To: sessions[0].ID, MessageID: id, Body: "ide body"})
	if err != nil || msg.Status != "pending_next_activity" {
		t.Fatalf("%+v %v", msg, err)
	}
	setup := runCursorHook(t, &service, map[string]any{"hook_event_name": "postToolUse", "conversation_id": conversation})
	if !strings.Contains(setup, "refreshed private capability") || strings.Contains(setup, "ide body") {
		t.Fatal("C12_SETUP_PRECEDES_PEER_CONTEXT")
	}
	out := runCursorHook(t, &service, map[string]any{"hook_event_name": "postToolUse", "conversation_id": conversation})
	if !strings.Contains(out, "additional_context") || !strings.Contains(out, "ide body") {
		t.Fatalf("later hook did not reach the started session: %s", out)
	}
	if msg := adapterTestMessage(t, store, id); msg.Status != "hook_context_emitted" {
		t.Fatalf("%+v", msg)
	}
	if sessions, err := store.List(); err != nil || len(sessions) != 2 || slices.ContainsFunc(sessions, func(v model.Session) bool { return v.ID == "cursor:"+session }) {
		t.Fatalf("session_id created a second actor: %+v %v", sessions, err)
	}
}

// runCursorHook runs one Cursor hook payload through the service and returns its output.
func runCursorHook(t *testing.T, service *bus.Service, fields map[string]any) string {
	t.Helper()
	input, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := service.Hook(t.Context(), "cursor", bytes.NewReader(input), &out); err != nil {
		t.Fatalf("%s: %v", fields["hook_event_name"], err)
	}
	return out.String()
}
