package bus

import (
	"context"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestActorAndCapabilityErrorsNameTheFix(t *testing.T) {
	store := newTestService(t).Store
	actor, _ := register(t, store, "cursor")
	for name, test := range map[string]struct {
		actor, key string
		code       model.ErrorCode
	}{
		"wrong capability": {actor.ID, "not the capability", model.CodeUnauthorized},
		"unknown actor":    {"cursor:" + newID(), "x", model.CodeNotFound},
		"malformed actor":  {"fixture", "x", model.CodeInvalidIdentity},
	} {
		_, err := store.authenticate(test.actor, test.key)
		if err == nil || model.ErrorCodeOf(err) != test.code || !strings.Contains(err.Error(), sessionStartHint) {
			t.Fatalf("%s: error = %v, want code %s and the SessionStart hint", name, err, test.code)
		}
		if strings.Contains(err.Error(), store.dir) || strings.Contains(err.Error(), ".json") {
			t.Fatalf("%s: error shows a file path: %v", name, err)
		}
	}
}

func TestStartupTextVoidsOldCapabilitiesAndLabelsBoundary(t *testing.T) {
	text := startupInstructions("codex:"+newID(), newID()+newID())
	for _, want := range []string{"72 characters", "earlier Zalki capability in your context is void", "Boundary is not a message ID"} {
		if !strings.Contains(text, want) {
			t.Fatalf("startup text lacks %q", want)
		}
	}
}

func TestAddressingErrorsSuggestTheFixWithoutPaths(t *testing.T) {
	service := newTestService(t)
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		return model.NewReceipt(model.StatusSubmitted)
	}
	from, key, to, toKey := registerPair(t, service.Store, "cursor", "codex")
	sent, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_, uuid, _ := strings.Cut(to.ID, ":")
	send := func(destination string) error {
		_, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: destination, Body: "hello"})
		return err
	}
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"short ID prefix":      {send("codex:" + uuid[:8]), "complete kind:UUID"},
		"bare hex has no kind": {send(uuid[:8]), "use list_agents"},
		"a plain name":         {send("claude"), "use list_agents"},
		"message ID as to":     {send(sent.ID), "reply_message"},
		"unrelated name":       {send("nobody-here"), "use list_agents"},
	} {
		if test.err == nil || !strings.Contains(test.err.Error(), test.want) {
			t.Fatalf("%s: error = %v, want text %q", name, test.err, test.want)
		}
	}
	if err := send("codex:" + uuid[:8]); strings.Contains(err.Error(), "list_agents for names") {
		t.Fatal("a short ID prefix got the generic hint")
	}
	_, replyErr := service.Reply(t.Context(), ReplyRequest{Actor: to.ID, Capability: toKey, MessageID: newID(), Body: "x"})
	_, foreignErr := service.Reply(t.Context(), ReplyRequest{Actor: from.ID, Capability: key, MessageID: sent.ID, Body: "x"})
	_, readErr := service.ReadMessages(MessageQuery{Actor: from.ID, Capability: key, MessageID: newID()})
	for name, test := range map[string]struct {
		err  error
		code model.ErrorCode
		want string
	}{
		"reply to an unknown ID":           {replyErr, model.CodeNotFound, "message not found; use the message ID"},
		"reply to a message not for you":   {foreignErr, model.CodeUnauthorized, "was not sent to you"},
		"list_messages with an unknown ID": {readErr, model.CodeNotFound, "message not found; use the message ID"},
	} {
		if test.err == nil || model.ErrorCodeOf(test.err) != test.code || !strings.Contains(test.err.Error(), test.want) {
			t.Fatalf("%s: error = %v, want code %s and %q", name, test.err, test.code, test.want)
		}
		if strings.Contains(test.err.Error(), service.Store.dir) || strings.Contains(test.err.Error(), ".json") {
			t.Fatalf("%s: error shows a file path: %v", name, test.err)
		}
	}
}
