package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func fixtureStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: testRegistry()}
}

func register(t *testing.T, store Store, kind string) (Session, string) {
	t.Helper()
	var endpoint *Endpoint
	if kind == "claude" {
		endpoint = &Endpoint{Path: "/fixture/not-a-real-socket"}
	}
	session, key, err := store.Register(kind, newID(), "/workspace", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return session, key
}

func TestConcurrentIdempotencyAcrossStoreInstances(t *testing.T) {
	store := fixtureStore(t)
	sender, key := register(t, store, "cursor")
	target, _ := register(t, store, "codex")
	id := newID()
	var calls atomic.Int32
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service := Service{Store: Store{Dir: store.Dir, Registry: testRegistry()}, Deliver: func(context.Context, Message, Session) Receipt {
				calls.Add(1)
				return Receipt{Status: "submitted"}
			}}
			_, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "same body"})
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("delivered %d times", calls.Load())
	}
}

func TestCapabilityScopeReplayAndCrashUncertainty(t *testing.T) {
	store := fixtureStore(t)
	sender, key := register(t, store, "cursor")
	target, _ := register(t, store, "codex")
	calls := 0
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		calls++
		return Receipt{Status: "unknown_after_timeout"}
	}}
	id := newID()
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: "wrong", To: target.ID, MessageID: id, Body: "test"}); err == nil {
		t.Fatal("accepted wrong capability")
	}
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "test"})
	if err != nil || msg.Status != "unknown_after_timeout" {
		t.Fatalf("%+v %v", msg, err)
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "changed"}); err == nil {
		t.Fatal("accepted changed replay")
	}
	crashID := newID()
	if _, _, fresh, err := store.ReserveMessage(SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: crashID, Body: "crash"}); err != nil || !fresh {
		t.Fatal(err)
	}
	msg, err = service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: crashID, Body: "crash"})
	if err != nil || msg.Status != "attempting" {
		t.Fatalf("lost crash uncertainty: %+v %v", msg, err)
	}
	if calls != 1 {
		t.Fatalf("retried an uncertain delivery: %d", calls)
	}
}

func TestRecipientAckRaceAndReplyScope(t *testing.T) {
	store := fixtureStore(t)
	sender, key := register(t, store, "cursor")
	target, targetKey := register(t, store, "codex")
	third, thirdKey := register(t, store, "codex")
	id := newID()
	service := Service{Store: store, Deliver: func(_ context.Context, msg Message, _ Session) Receipt {
		if _, err := store.Ack(third.ID, thirdKey, msg.ID); err == nil {
			t.Error("third-party ACK accepted")
		}
		if _, err := store.Ack(target.ID, targetKey, msg.ID); err != nil {
			t.Error(err)
		}
		return Receipt{Status: "submitted"}
	}}
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "hello"})
	if err != nil || msg.AcknowledgedAt == "" {
		t.Fatalf("ACK racing with receipt was lost: %+v %v", msg, err)
	}
	if _, _, _, err := store.ReserveMessage(SendRequest{Actor: third.ID, Capability: thirdKey, To: sender.ID, MessageID: newID(), Body: "reply", ReplyTo: id}); err == nil {
		t.Fatal("third party replied to someone else's request")
	}
	if _, _, fresh, err := store.ReserveMessage(SendRequest{Actor: target.ID, Capability: targetKey, To: sender.ID, MessageID: newID(), Body: "reply", ReplyTo: id}); err != nil || !fresh {
		t.Fatal(err)
	}
}

func TestApprovalAndRetirementPreventTransport(t *testing.T) {
	store := fixtureStore(t)
	sender, key := register(t, store, "cursor")
	target, targetKey := register(t, store, "codex")
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		t.Fatal("transport used while blocked")
		return Receipt{}
	}}
	if err := store.Update(target.ID, "approval", false); err != nil {
		t.Fatal(err)
	}
	id := newID()
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "test"})
	if err != nil || msg.Status != "not_sent_pending_approval" {
		t.Fatalf("%+v %v", msg, err)
	}
	if _, err := store.Ack(target.ID, targetKey, id); err == nil {
		t.Fatal("ACK accepted for unsent message")
	}
	if err := store.Update(target.ID, "ready", false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: id, Body: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(target.ID, "retired", true); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "test"}); err == nil {
		t.Fatal("sent to retired session")
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].KeyHash != "" {
		t.Fatalf("retired session or secret leaked: %+v", sessions)
	}
}

func TestClaudeTransportEnabledByDefault(t *testing.T) {
	store := fixtureStore(t)
	sender, key := register(t, store, "codex")
	target, _ := register(t, store, "claude")
	delivered := false
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		delivered = true
		return Receipt{Status: StatusTransportWritten}
	}}
	msg, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "test"})
	if err != nil || msg.Status != StatusTransportWritten || !delivered {
		t.Fatalf("Claude was not enabled by default: %+v %v", msg, err)
	}
}

func TestCursorPromptReactivatesResumedConversation(t *testing.T) {
	store := fixtureStore(t)
	target, key := register(t, store, "cursor")
	service := Service{Store: store}
	callHook := func(id, event string) {
		t.Helper()
		input, _ := json.Marshal(map[string]any{
			"conversation_id": strings.TrimPrefix(id, "cursor:"),
			"hook_event_name": event, "status": "completed",
		})
		var out bytes.Buffer
		if err := service.Hook("cursor", bytes.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatal("unexpected context or automatic follow-up")
		}
	}
	callHook(target.ID, "sessionEnd")
	callHook(target.ID, "postToolUse")
	callHook(target.ID, "stop")
	if sessions, err := store.List(); err != nil || len(sessions) != 0 {
		t.Fatalf("background lifecycle events revived a closed session: %+v %v", sessions, err)
	}
	callHook(target.ID, "beforeSubmitPrompt")
	sessions, err := store.List()
	if err != nil || len(sessions) != 1 || sessions[0].ID != target.ID || sessions[0].Generation != target.Generation || sessions[0].State != "busy" {
		t.Fatalf("resumed conversation identity was not retained: %+v %v", sessions, err)
	}
	if _, err := store.Messages(target.ID, key); err != nil {
		t.Fatalf("resumed conversation lost its original capability: %v", err)
	}
	callHook("cursor:"+newID(), "beforeSubmitPrompt")
	if sessions, err := store.List(); err != nil || len(sessions) != 1 {
		t.Fatalf("prompt registered an unknown conversation: %+v %v", sessions, err)
	}
}

func TestEndpointRejectsFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "regular")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectEndpoint(path); err == nil {
		t.Fatal("accepted regular file")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectEndpoint(link); err == nil {
		t.Fatal("accepted symlink")
	}
}
