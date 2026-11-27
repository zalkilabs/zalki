package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters/antigravity"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// These tests run the Antigravity adapter through the real hook service with a
// registry that holds only this client.
// The payloads have the keys agy 1.5.18 sent in a live run (names and types; the
// values are made up).

func antigravityStore(t *testing.T) bus.Store {
	t.Helper()
	registry, err := agent.NewRegistry(antigravity.New())
	if err != nil {
		t.Fatal(err)
	}
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// antigravityPayload is a hook payload with the keys of the live run.
func antigravityPayload(event, conversation string, extra map[string]any) map[string]any {
	payload := map[string]any{
		"hookEventName": event, "conversationId": conversation, "cwd": "/workspace",
		"workspacePaths": []string{"/workspace"}, "transcriptPath": "/fixture/transcript.jsonl", "artifactDirectoryPath": "/fixture/artifacts",
		"modelName": "fixture-model", "lastUserInput": "hello",
	}
	switch event {
	case "PreInvocation":
		payload["invocationNum"], payload["initialNumSteps"] = 0, 3
	case "Stop":
		payload["executionNum"], payload["terminationReason"], payload["error"] = 0, "NO_TOOL_CALL", ""
		payload["fullyIdle"], payload["continuationActive"] = true, false
	}
	for key, value := range extra {
		payload[key] = value
	}
	return payload
}

func antigravityHook(t *testing.T, service bus.Service, payload map[string]any) (string, error) {
	t.Helper()
	input, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = service.Hook(t.Context(), "antigravity", bytes.NewReader(input), &out)
	return out.String(), err
}

// antigravityPair registers a sender and a receiving conversation and sends one
// message to the receiver. It returns the receiver and the message id.
type antigravityChat struct {
	service  bus.Service
	store    bus.Store
	sender   model.Session
	key      string
	receiver model.Session
}

func newAntigravityChat(t *testing.T) antigravityChat {
	t.Helper()
	store := antigravityStore(t)
	sender, key := registerAdapterSession(t, store, "antigravity")
	receiver, _ := registerAdapterSession(t, store, "antigravity")
	return antigravityChat{service: bus.Service{Store: store}, store: store, sender: sender, key: key, receiver: receiver}
}

func (c antigravityChat) send(t *testing.T, body string) string {
	t.Helper()
	id := model.NewUUID()
	if _, err := c.service.Send(context.Background(), bus.SendRequest{Actor: c.sender.ID, Capability: c.key, To: c.receiver.ID, MessageID: id, Body: body}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (c antigravityChat) hook(t *testing.T, event string, extra map[string]any) string {
	t.Helper()
	out, err := antigravityHook(t, c.service, antigravityPayload(event, c.receiver.NativeID, extra))
	if err != nil {
		t.Fatalf("%s hook: %v", event, err)
	}
	return out
}

func (c antigravityChat) continues(t *testing.T) int {
	t.Helper()
	var state struct {
		Continues int `json:"continues"`
	}
	if err := json.Unmarshal(adapterTestSession(t, c.store, c.receiver.ID).AdapterState, &state); err != nil {
		t.Fatal(err)
	}
	return state.Continues
}

func requireReply(t *testing.T, out, prefix, wantBody string) {
	t.Helper()
	if !strings.HasPrefix(out, prefix) || !strings.HasSuffix(out, "\"}}]}\n") && !strings.HasSuffix(out, "\"}\n") {
		t.Fatalf("reply has the wrong framing: %q", out)
	}
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, wantBody) {
		t.Fatalf("reply is not one line with the message: %q", out)
	}
}

const (
	injectPrefix   = `{"injectSteps":[{"systemMessage":{"systemMessage":"`
	continuePrefix = `{"decision":"continue","reason":"`
)

func TestAntigravitySessionStartRegistersTheConversationAndInjectsItsInstructions(t *testing.T) {
	store := antigravityStore(t)
	service := bus.Service{Store: store}
	conversation := model.NewUUID()
	out, err := antigravityHook(t, service, antigravityPayload("SessionStart", conversation, nil))
	if err != nil || !strings.HasPrefix(out, injectPrefix) || !strings.HasSuffix(out, "\"}}]}\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("start output %q, error %v", out, err)
	}
	var reply struct {
		Steps []struct {
			Message struct {
				Text string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil || len(reply.Steps) != 1 || !strings.Contains(reply.Steps[0].Message.Text, "Zalki actor: antigravity:"+conversation) {
		t.Fatalf("the system message does not carry the instructions of this conversation: %q (%v)", out, err)
	}
	sessions, err := store.List()
	if err != nil || len(sessions) != 1 || sessions[0].ID != "antigravity:"+conversation || sessions[0].Kind != "antigravity" {
		t.Fatalf("sessions after the start hook = %+v, %v", sessions, err)
	}
	// The event key is read ignoring case.
	second := antigravityPayload("SessionStart", model.NewUUID(), nil)
	delete(second, "hookEventName")
	second["HOOKEVENTNAME"] = "SessionStart"
	if out, err := antigravityHook(t, service, second); err != nil || !strings.HasPrefix(out, injectPrefix) {
		t.Fatalf("a payload with the event key in capitals was not accepted: %q, %v", out, err)
	}
}

func TestAntigravityDeliversPendingMessagesBeforeTheNextModelCall(t *testing.T) {
	chat := newAntigravityChat(t)
	id := chat.send(t, "BODY-ONE")
	if status := adapterTestMessage(t, chat.store, id).Status; status != "pending_next_activity" {
		t.Fatalf("the message status = %q, want pending_next_activity: Antigravity has no live path", status)
	}
	out := chat.hook(t, "PreInvocation", nil)
	requireReply(t, out, injectPrefix, "BODY-ONE")
	if !strings.Contains(out, "[zalki message "+id+"]") {
		t.Fatalf("the system message does not name the message: %q", out)
	}
	if message := adapterTestMessage(t, chat.store, id); message.Status != "hook_context_emitted" || message.AcknowledgedAt != "" {
		t.Fatalf("after the hook: %+v (emitted context is not an acknowledgement)", message)
	}
	if got := adapterTestSession(t, chat.store, chat.receiver.ID).State; got != model.StateBusy {
		t.Fatalf("state = %q, want busy: core marks the session busy before a model call", got)
	}
	if out := chat.hook(t, "PreInvocation", nil); out != "" {
		t.Fatalf("the same message was delivered twice: %q", out)
	}
}

func TestAntigravityWritesNothingBeforeAModelCallWhenNothingIsPending(t *testing.T) {
	chat := newAntigravityChat(t)
	if out := chat.hook(t, "PreInvocation", nil); out != "" {
		t.Fatalf("stdout = %q, want nothing", out)
	}
	if got := adapterTestSession(t, chat.store, chat.receiver.ID).State; got != model.StateBusy {
		t.Fatalf("state = %q, want busy", got)
	}
}

// A normal end of a turn with a message continues the turn, and the session is ready.
func TestAntigravityContinuesAFinishedTurnWithAPendingMessage(t *testing.T) {
	chat := newAntigravityChat(t)
	id := chat.send(t, "BODY-STOP")
	out := chat.hook(t, "Stop", nil)
	requireReply(t, out, continuePrefix, "BODY-STOP")
	if !strings.Contains(out, "[zalki message "+id+"]") || chat.continues(t) != 1 {
		t.Fatalf("reply %q, continues %d", out, chat.continues(t))
	}
	if got := adapterTestSession(t, chat.store, chat.receiver.ID).State; got != model.StateReady {
		t.Fatalf("state = %q, want ready", got)
	}
}

// A Stop with nothing to deliver ends the turn and resets the count.
func TestAntigravityEndsTheTurnAndResetsTheCountWhenNothingIsPending(t *testing.T) {
	chat := newAntigravityChat(t)
	chat.send(t, "first")
	chat.hook(t, "Stop", nil)
	chat.send(t, "second")
	chat.hook(t, "Stop", nil)
	if got := chat.continues(t); got != 2 {
		t.Fatalf("continues = %d, want 2", got)
	}
	if out := chat.hook(t, "Stop", nil); out != "" || chat.continues(t) != 0 {
		t.Fatalf("a Stop with nothing pending wrote %q and left the count at %d", out, chat.continues(t))
	}
}

// At most five continues in a row: the sixth Stop ends the turn, keeps the message
// for the next turn and resets the count.
func TestAntigravityRefusesTheSixthContinueInARow(t *testing.T) {
	chat := newAntigravityChat(t)
	for i := 1; i <= 5; i++ {
		chat.send(t, "BODY-"+strings.Repeat("N", i))
		if out := chat.hook(t, "Stop", nil); !strings.HasPrefix(out, continuePrefix) || chat.continues(t) != i {
			t.Fatalf("continue %d: reply %q, count %d", i, out, chat.continues(t))
		}
	}
	chat.send(t, "BODY-SIXTH")
	if out := chat.hook(t, "Stop", nil); out != "" || chat.continues(t) != 0 {
		t.Fatalf("the sixth continue in a row was not refused: reply %q, count %d", out, chat.continues(t))
	}
	if got := adapterTestSession(t, chat.store, chat.receiver.ID).State; got != model.StateReady {
		t.Fatalf("state = %q, want ready", got)
	}
	out := chat.hook(t, "Stop", nil)
	requireReply(t, out, continuePrefix, "BODY-SIXTH")
	if chat.continues(t) != 1 {
		t.Fatalf("the message that was kept for the next turn was delivered with count %d, want 1", chat.continues(t))
	}
}

// A turn that did not end normally is an interrupt: nothing is continued, the
// message stays pending, the session is ready and the count is reset.
func TestAntigravityDoesNotContinueATurnThatDidNotEndNormally(t *testing.T) {
	for _, reason := range []string{"USER_CANCELED", "ERROR", "MAX_INVOCATIONS", "MAX_FORCED_INVOCATIONS", "MAX_TOKEN_BUDGET_EXCEEDED", "TERMINAL_STEP_TYPE", "TERMINAL_CUSTOM_HOOK", "INJECTED_RESPONSE"} {
		t.Run(reason, func(t *testing.T) {
			chat := newAntigravityChat(t)
			chat.send(t, "first")
			chat.hook(t, "Stop", nil)
			id := chat.send(t, "BODY-KEPT")
			if out := chat.hook(t, "Stop", map[string]any{"terminationReason": reason}); out != "" || chat.continues(t) != 0 {
				t.Fatalf("reply %q, count %d", out, chat.continues(t))
			}
			if status := adapterTestMessage(t, chat.store, id).Status; status != "pending_next_activity" {
				t.Fatalf("the message status = %q: an interrupted turn must not consume it", status)
			}
			if got := adapterTestSession(t, chat.store, chat.receiver.ID).State; got != model.StateReady {
				t.Fatalf("state = %q, want ready", got)
			}
		})
	}
}

func TestAntigravityTreatsAStopWithoutAReasonAsANormalEnd(t *testing.T) {
	chat := newAntigravityChat(t)
	chat.send(t, "BODY-NOREASON")
	payload := antigravityPayload("Stop", chat.receiver.NativeID, nil)
	delete(payload, "terminationReason")
	out, err := antigravityHook(t, chat.service, payload)
	if err != nil {
		t.Fatal(err)
	}
	requireReply(t, out, continuePrefix, "BODY-NOREASON")
}

func TestAntigravityIgnoresAConversationItDoesNotKnow(t *testing.T) {
	service := bus.Service{Store: antigravityStore(t)}
	for _, event := range []string{"PreInvocation", "Stop"} {
		if out, err := antigravityHook(t, service, antigravityPayload(event, model.NewUUID(), nil)); err != nil || out != "" {
			t.Fatalf("%s for an unknown conversation: %q, %v", event, out, err)
		}
	}
}

func TestAntigravityRefusesAPayloadWithoutAConversationOrWithAWrongType(t *testing.T) {
	chat := newAntigravityChat(t)
	missing := antigravityPayload("PreInvocation", chat.receiver.NativeID, nil)
	delete(missing, "conversationId")
	wrong := antigravityPayload("PreInvocation", chat.receiver.NativeID, map[string]any{"conversationId": 7})
	for name, payload := range map[string]map[string]any{"no conversation id": missing, "a number as conversation id": wrong} {
		if out, err := antigravityHook(t, chat.service, payload); err == nil || out != "" {
			t.Fatalf("%s: output %q, error %v; want a refusal and no output", name, out, err)
		}
	}
}
