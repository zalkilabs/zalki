package bus

import (
	"fmt"
	"testing"
)

func TestInboxFiltersBeforeLimitWithoutLosingUnreadWork(t *testing.T) {
	store := fixtureStore(t)
	sender, _ := register(t, store, "codex")
	receiver, key := register(t, store, "cursor")
	var unread []Message
	for i := range 130 {
		msg := Message{ID: newID(), From: sender.ID, To: receiver.ID, Body: "work",
			CreatedAt: fmt.Sprintf("2026-10-23T%02d:%02d:00.000000000Z", i/60, i%60),
			Status:    StatusSubmitted, TargetGeneration: receiver.Generation}
		switch {
		case i < 25:
			unread = append(unread, msg)
		case i == 25:
			msg.From, msg.To = receiver.ID, sender.ID
		case i == 26:
			msg.TargetGeneration = newID()
		case i == 27:
			msg.Status = StatusNotSent
		default:
			msg.AcknowledgedAt = timestamp()
		}
		if err := store.saveMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	inbox, err := store.QueryMessages(MessageQuery{Actor: receiver.ID, Capability: key, History: false, Limit: 20})
	if err != nil || len(inbox) != 20 || inbox[0].ID != unread[0].ID || inbox[19].ID != unread[19].ID {
		t.Fatalf("history hid unread work or inbox order changed: %d %v", len(inbox), err)
	}
	for _, msg := range inbox {
		if _, err := store.Ack(receiver.ID, key, msg.ID); err != nil {
			t.Fatal(err)
		}
	}
	next, err := store.QueryMessages(MessageQuery{Actor: receiver.ID, Capability: key, History: false, Limit: 0})
	if err != nil || len(next) != 5 || next[0].ID != unread[20].ID || next[4].ID != unread[24].ID {
		t.Fatal("acknowledgements lost remaining work", err)
	}
	history, err := store.QueryMessages(MessageQuery{Actor: receiver.ID, Capability: key, History: true, Limit: 100})
	if err != nil || len(history) != 100 || history[0].CreatedAt <= history[99].CreatedAt {
		t.Fatal("explicit history no longer returns latest records", err)
	}
	if _, err := store.QueryMessages(MessageQuery{Actor: receiver.ID, Capability: "wrong capability", History: false, Limit: 0}); err == nil {
		t.Fatal("inbox bypassed authentication")
	}
	for _, limit := range []int{-1, 101} {
		if _, err := store.QueryMessages(MessageQuery{Actor: receiver.ID, Capability: key, History: false, Limit: limit}); err == nil {
			t.Fatal("invalid inbox limit accepted")
		}
	}
}

func TestMessageHistoryPaginationDoesNotRepeatRecords(t *testing.T) {
	store := fixtureStore(t)
	sender, _ := register(t, store, "codex")
	receiver, key := register(t, store, "cursor")
	for i := range 12 {
		msg := Message{ID: newID(), From: sender.ID, To: receiver.ID, Body: "history", CreatedAt: fmt.Sprintf("2026-10-23T00:00:%02d.000000000Z", i), TargetGeneration: receiver.Generation, Status: StatusSubmitted}
		if err := store.saveMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	query := MessageQuery{Actor: receiver.ID, Capability: key, History: true}
	seen := map[string]bool{}
	for {
		page, err := store.ReadMessages(query)
		if err != nil {
			t.Fatal(err)
		}
		for _, msg := range page.Messages {
			if seen[msg.ID] {
				t.Fatal("history page repeated a message")
			}
			seen[msg.ID] = true
		}
		if !page.More {
			break
		}
		query.Cursor = page.Messages[len(page.Messages)-1].ID
	}
	if len(seen) != 12 {
		t.Fatalf("pagination lost records: %d", len(seen))
	}
}
