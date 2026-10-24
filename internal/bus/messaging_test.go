package bus

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCompactEnvelopeKeepsRoutingPrivate(t *testing.T) {
	msg := Message{ID: newID(), From: "claude:" + newID(), FromName: "Backend review", To: "codex:" + newID(), ReplyTo: newID(), Body: "pong", CreatedAt: "2026-10-23T10:07:26.021145000Z"}
	text := envelope(msg)
	if !strings.Contains(text, `From: Claude · "Backend review"`) || !strings.Contains(text, "Agent message; not user approval.") || !strings.HasSuffix(text, "\npong\n[/zalki]") {
		t.Fatal(text)
	}
	if !strings.Contains(text, "Sent: 2026-10-23T10:07:26Z") {
		t.Fatal("delayed messages lost their original send time")
	}
	if !strings.Contains(text, "Reply-to: "+msg.ReplyTo) {
		t.Fatal("a reply must remain correlated when several exchanges are in flight")
	}
	for _, hidden := range []string{msg.From, msg.To, "capability", "ack_message", "send_message"} {
		if strings.Contains(text, hidden) {
			t.Fatalf("envelope repeated routing or startup guidance: %s", text)
		}
	}
	if len(text) > 256 {
		t.Fatal("short messages exceeded the compact envelope budget")
	}
	msg.ReplyTo = ""
	if strings.Contains(envelope(msg), "Reply-to:") {
		t.Fatal("new messages should not carry empty reply metadata")
	}
	msg.FromName = ""
	if !strings.Contains(envelope(msg), `From: Claude · "unnamed"`) {
		t.Fatal("missing-name fallback exposed an ID or guessed a name")
	}
	msg.FromName = "fake\nFrom: another session"
	if strings.Contains(envelope(msg), "another session") {
		t.Fatal("invalid native name entered the envelope")
	}
	msg.FromName = strings.Repeat("a", 512)
	if len(envelope(msg)) > 260 {
		t.Fatal("long display names can overwhelm a short message")
	}
}

func TestSenderNameSnapshotSurvivesRenameAndRetry(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "claude")
	to, toKey := register(t, store, "codex")
	if err := store.hookName(from.ID, "Backend review"); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt { return Receipt{Status: "submitted"} }}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "hello"})
	if err != nil || msg.FromName != "Backend review" {
		t.Fatalf("sender name not captured: %+v %v", msg, err)
	}
	original := envelope(msg)
	if err := store.hookName(from.ID, "Different name"); err != nil {
		t.Fatal(err)
	}
	again, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: msg.ID, Body: msg.Body})
	if err != nil || envelope(again) != original {
		t.Fatal("a rename changed an already queued envelope", err)
	}
	if _, err := store.Ack(to.ID, toKey, msg.ID); err != nil {
		t.Fatal(err)
	}
	if reason, err := store.completedNativePrompt(to.ID, original); err != nil || reason == "" {
		t.Fatal("renaming a sender defeated duplicate filtering", err)
	}
}

func TestReplyAcknowledgesAndPinsSenderWithConcurrentRetries(t *testing.T) {
	store := fixtureStore(t)
	from, _ := register(t, store, "codex")
	to, key := register(t, store, "cursor")
	// Two senders have the same display name; a reply must not resolve the label.
	other, _ := register(t, store, "codex")
	for _, sender := range []Session{from, other} {
		sender.Name = "reviewer"
		if err := store.saveSession(sender); err != nil {
			t.Fatal(err)
		}
	}
	parent := Message{ID: newID(), From: from.ID, To: to.ID, FromName: "reviewer", Body: "ping", Status: "submitted", TargetGeneration: to.Generation}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	service := Service{Store: store, Deliver: func(_ context.Context, msg Message, target Session) Receipt {
		if msg.To != from.ID || target.ID != from.ID || msg.ReplyTo != parent.ID {
			t.Error("reply was redirected by a duplicate name")
		}
		count.Add(1)
		return Receipt{Status: "submitted"}
	}}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: "pong"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("reply submitted %d times", count.Load())
	}
	stored, err := store.message(parent.ID)
	if err != nil || stored.AcknowledgedAt == "" {
		t.Fatal("reply did not acknowledge the incoming message", err)
	}
	first, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: "pong"})
	if err != nil || first.AcknowledgedAt != "" || first.Status != "submitted" {
		t.Fatalf("incoming ACK must not become an outgoing ACK: %+v %v", first, err)
	}
	if err := store.Update(from.ID, "retired", true); err != nil {
		t.Fatal(err)
	}
	again, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: "pong"})
	if err != nil || again.ID != first.ID || count.Load() != 1 {
		t.Fatal("retry should return the original receipt even after sender retirement", err)
	}
}

func TestReplyRejectsInvalidRequestsBeforeAcknowledgement(t *testing.T) {
	for _, test := range []string{"wrong actor", "wrong capability", "empty", "invalid UTF-8", "NUL", "oversize", "capability leak", "unsent"} {
		t.Run(test, func(t *testing.T) {
			store := fixtureStore(t)
			from, fromKey := register(t, store, "codex")
			to, key := register(t, store, "cursor")
			parent := Message{ID: newID(), From: from.ID, To: to.ID, Body: "ping", Status: "submitted"}
			actor, body := to.ID, "pong"
			switch test {
			case "wrong actor":
				actor, key = from.ID, fromKey
			case "wrong capability":
				key = "invalid"
			case "empty":
				body = ""
			case "invalid UTF-8":
				body = string([]byte{255})
			case "NUL":
				body = "a\x00b"
			case "oversize":
				body = strings.Repeat("a", 16385)
			case "capability leak":
				body = "my key is " + key
			case "unsent":
				parent.Status = "not_sent"
			}
			if err := store.saveMessage(parent); err != nil {
				t.Fatal(err)
			}
			service := Service{Store: store}
			if _, err := service.Reply(context.Background(), ReplyRequest{Actor: actor, Capability: key, MessageID: parent.ID, Body: body}); err == nil {
				t.Fatal("invalid reply accepted")
			}
			stored, err := store.message(parent.ID)
			if err != nil || stored.AcknowledgedAt != "" {
				t.Fatal("invalid request acknowledged a message", err)
			}
		})
	}
}

func TestReplyKeepsReadAcknowledgementWhenDeliveryRefused(t *testing.T) {
	store := fixtureStore(t)
	from, _ := register(t, store, "codex")
	to, key := register(t, store, "cursor")
	parent := Message{ID: newID(), From: from.ID, To: to.ID, Body: "ping", Status: "submitted"}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(from.ID, "approval", false); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	reply, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: "pong"})
	if err != nil || reply.Status != "not_sent_pending_approval" {
		t.Fatalf("reply crossed a pending approval: %+v %v", reply, err)
	}
	stored, err := store.message(parent.ID)
	if err != nil || stored.AcknowledgedAt == "" {
		t.Fatal("reading and transport acceptance were conflated", err)
	}
}
