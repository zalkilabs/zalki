package bus

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReplyRecoveryAtEachDurableBoundary(t *testing.T) {
	for _, stage := range []string{"intent", "reservation", "ack", "receipt"} {
		t.Run(stage, func(t *testing.T) {
			store := fixtureStore(t)
			from, _ := register(t, store, "codex")
			to, key := register(t, store, "cursor")
			parent := Message{ID: newID(), From: from.ID, To: to.ID, Body: "ping", Status: StatusSubmitted}
			if err := store.saveMessage(parent); err != nil {
				t.Fatal(err)
			}
			var reply Message
			if err := store.locked(func() error {
				var err error
				reply, _, _, err = store.prepareMessage(SendRequest{Actor: to.ID, Capability: key, To: from.ID, MessageID: replyID(to.ID, parent.ID, "pong"), Body: "pong", ReplyTo: parent.ID}, false)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			tx := replyTransaction{Schema: 1, ParentID: parent.ID, AcknowledgedAt: timestamp(), Reply: reply}
			dir := filepath.Join(store.Dir, "transactions")
			if err := privateDir(dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, reply.ID+".json")
			if err := writeJSON(path, tx); err != nil {
				t.Fatal(err)
			}
			// Persist exactly the files a process may leave at each interruption.
			if stage != "intent" {
				if err := store.saveMessage(reply); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "ack" || stage == "receipt" {
				parent.AcknowledgedAt = tx.AcknowledgedAt
				if err := store.saveMessage(parent); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "receipt" {
				reply.Status = StatusSubmitted
				reply.AcknowledgedAt = timestamp()
				if err := store.saveMessage(reply); err != nil {
					t.Fatal(err)
				}
			}
			// A fresh Store represents a restarted process. Its first journal read
			// completes the transaction before exposing either half to callers.
			restarted := Store{Dir: store.Dir}
			if _, err := restarted.List(); err != nil {
				t.Fatal(err)
			}
			ack, err := restarted.message(parent.ID)
			if err != nil || ack.AcknowledgedAt != tx.AcknowledgedAt {
				t.Fatal("missing recovered ACK", err)
			}
			reserved, err := restarted.message(reply.ID)
			if err != nil || reserved.Body != "pong" || reserved.To != from.ID || reserved.Status != reply.Status || reserved.AcknowledgedAt != reply.AcknowledgedAt {
				t.Fatal("reply intent or receipt lost", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("committed intent not cleared", err)
			}
			calls := 0
			service := Service{Store: restarted, Deliver: func(context.Context, Message, Session) Receipt { calls++; return Receipt{Status: StatusSubmitted} }}
			if _, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: "pong"}); err != nil || calls != 0 {
				t.Fatal("recovery resubmitted an uncertain reply", err)
			}
		})
	}
}

func TestReplyCannotAcknowledgeWithoutDurableIntent(t *testing.T) {
	store := fixtureStore(t)
	from, _ := register(t, store, "codex")
	to, key := register(t, store, "cursor")
	parent := Message{ID: newID(), From: from.ID, To: to.ID, Body: "ping", Status: StatusSubmitted}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	// Prevent creation of the private transaction directory.
	if err := os.WriteFile(filepath.Join(store.Dir, "transactions"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	if _, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: "pong"}); err == nil {
		t.Fatal("reply ignored intent write failure")
	}
	stored, err := store.message(parent.ID)
	if err != nil || stored.AcknowledgedAt != "" {
		t.Fatal("ACK persisted without reply intent", err)
	}
	if _, err := store.message(replyID(to.ID, parent.ID, "pong")); !os.IsNotExist(err) {
		t.Fatal("partial reply reservation", err)
	}
}

func TestReplyRecordsUnavailableDestinationWithAcknowledgement(t *testing.T) {
	for _, missing := range []bool{false, true} {
		store := fixtureStore(t)
		from, _ := register(t, store, "codex")
		to, key := register(t, store, "cursor")
		parent := Message{ID: newID(), From: from.ID, To: to.ID, Body: "ping", Status: StatusSubmitted}
		if err := store.saveMessage(parent); err != nil {
			t.Fatal(err)
		}
		if missing {
			path, _ := store.sessionPath(from.ID)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else if err := store.Update(from.ID, StateRetired, true); err != nil {
			t.Fatal(err)
		}
		service := Service{Store: store}
		reply, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: "pong"})
		if err != nil || reply.Status != StatusNotSent || reply.Receipt == nil || reply.Receipt.Code == "" {
			t.Fatal("missing failed reply reservation", reply, err)
		}
		ack, err := store.message(parent.ID)
		if err != nil || ack.AcknowledgedAt == "" {
			t.Fatal("read acknowledgement lost", err)
		}
		again, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: "pong"})
		if err != nil || again.ID != reply.ID || again.Status != StatusNotSent {
			t.Fatal("failed reply retried", err)
		}
	}
}
