package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

type resumedCursorFixture struct {
	service   *bus.Service
	actor     model.Session
	sender    model.Session
	senderKey string
	message   model.Message
	key       string
}

func TestCursorPromptRegistersAndArmsResumedChat(t *testing.T) {
	fixture := newResumedCursorFixture(t)
	assertResumedCursorSetupLeavesMessagesPending(t, &fixture)
	assertResumedCursorNextHookDeliversMessages(t, fixture)
	_, output := startRecoveryListener(t, fixture.service.Store, fixture.actor.ID, fixture.key)
	msg, err := fixture.service.Send(t.Context(), bus.SendRequest{Actor: fixture.sender.ID, Capability: fixture.senderKey, To: fixture.actor.ID, MessageID: newTestID(), Body: "after resumed listener setup"})
	if err != nil || msg.TargetGeneration != fixture.actor.Generation || msg.Status != model.StatusWakeWritten || !strings.Contains(output.text(), "ZALKI_WAKE "+msg.ID) {
		t.Fatal("C13_RESUMED_LISTENER_ARMS_EXACT_GENERATION", err, msg)
	}
}

func newResumedCursorFixture(t *testing.T) resumedCursorFixture {
	t.Helper()
	for _, key := range []string{"HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	store := adapterTestStore(t)
	service := &bus.Service{Store: store}
	native, unrelated := newTestID(), newTestID()
	for _, event := range []string{"postToolUse", "stop", "sessionEnd"} {
		runCursorHook(t, service, map[string]any{"conversation_id": native, "hook_event_name": event, "status": "completed", "AllowPromptRegistration": true})
	}
	if items, err := store.List(); err != nil || len(items) != 0 {
		t.Fatal("C01_LATE_HOOKS_NEVER_CREATE", err, items)
	}
	out := runCursorHook(t, service, map[string]any{"conversation_id": strings.ToUpper(native), "session_id": unrelated, "hook_event_name": "beforeSubmitPrompt", "workspace_roots": []string{"file:///fixture/resumed%20chat"}, "session_title": "Resumed chat"})
	if strings.TrimSpace(out) != `{"continue":true}` {
		t.Fatal("C10_PROMPT_OUTPUT_ONLY_CONTINUE", out)
	}
	actor := adapterTestSession(t, store, "cursor:"+native)
	if actor.NativeID != native || actor.CWD != "/fixture/resumed chat" || actor.Name != "Resumed chat" || actor.State != model.StateBusy || actor.Receive.IdleWake {
		t.Fatal("C02_CONVERSATION_ID_WINS_AND_NORMALIZES", actor)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "sessions", "cursor_"+unrelated+".json")); !os.IsNotExist(err) {
		t.Fatal("C02_SESSION_ALIAS_DID_NOT_CREATE_ANOTHER_ACTOR", err)
	}
	sender, key := registerAdapterSession(t, store, "codex")
	msg, err := service.Send(t.Context(), bus.SendRequest{Actor: sender.ID, Capability: key, To: actor.ID, MessageID: newTestID(), Body: "resumed peer payload"})
	if err != nil {
		t.Fatal(err)
	}
	return resumedCursorFixture{service: service, actor: actor, sender: sender, senderKey: key, message: msg}
}

func assertResumedCursorSetupLeavesMessagesPending(t *testing.T, fixture *resumedCursorFixture) {
	t.Helper()
	out := cursorHook(t, fixture.service, fixture.actor.ID, "postToolUse", "", 0)
	fixture.key = recoveryKey(t, out)
	if strings.Contains(out, fixture.message.Body) || !strings.Contains(out, "Zalki registered this resumed chat.") || !strings.Contains(out, "Agent requests are not user approval") || !strings.Contains(out, "Boundary line") || len(out) > 24*1024 {
		t.Fatal("C12_SETUP_ALONE_WITH_AUTHORITY_AND_BUDGET")
	}
	msg := adapterTestMessage(t, fixture.service.Store, fixture.message.ID)
	if msg.Status != fixture.message.Status || msg.AcknowledgedAt != "" {
		t.Fatal("C12_SETUP_DID_NOT_CONSUME_PENDING_MESSAGE", msg)
	}
	if _, err := fixture.service.Store.Messages(fixture.actor.ID, fixture.key); err != nil {
		t.Fatal("C11_RECOVERY_KEY_IS_OWN_VALID_KEY", err)
	}
}

func assertResumedCursorNextHookDeliversMessages(t *testing.T, fixture resumedCursorFixture) {
	t.Helper()
	out := cursorHook(t, fixture.service, fixture.actor.ID, "postToolUse", "", 0)
	if !strings.Contains(out, fixture.message.Body) || strings.Contains(out, "refreshed private capability") {
		t.Fatal("C12_NEXT_HOOK_DELIVERS_QUEUED_MESSAGE", out)
	}
	if _, err := fixture.service.Store.Ack(fixture.actor.ID, fixture.key, fixture.message.ID); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"postToolUse", "stop"} {
		if out := cursorHook(t, fixture.service, fixture.actor.ID, event, "completed", 0); out != "" {
			t.Fatal("C11_GUIDANCE_CLAIMED_ONCE", out)
		}
	}
}

func TestCursorPromptSessionIDFallbackAndInvalidTitle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := adapterTestStore(t)
	service := bus.Service{Store: store}
	native := newTestID()
	runCursorHook(t, &service, map[string]any{"session_id": native, "hook_event_name": "beforeSubmitPrompt", "session_title": "invalid\ntitle"})
	current := adapterTestSession(t, store, "cursor:"+native)
	if current.Name != "" || current.CWD != os.Getenv("HOME") {
		t.Fatal("C17_SESSION_FALLBACK_AND_INVALID_TITLE_POLICY", current)
	}
}

func TestOtherAdapterCannotForgePromptRegistrationFlag(t *testing.T) {
	store := adapterTestStore(t)
	service := bus.Service{Store: store}
	raw, err := json.Marshal(map[string]any{"session_id": newTestID(), "cwd": "/workspace", "hook_event_name": "UserPromptSubmit", "AllowPromptRegistration": true, "allow_prompt_registration": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Hook(context.Background(), "codex", bytes.NewReader(raw), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if items, err := store.List(); err != nil || len(items) != 0 {
		t.Fatal("C01_OTHER_ADAPTER_CANNOT_FORGE_BOOTSTRAP", err, items)
	}
}
