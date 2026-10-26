package bus

import (
	"context"
	"strings"
	"testing"
)

func TestReceiptReadDoesNotCreateSecondNativeInbox(t *testing.T) {
	for _, kind := range []string{"codex", "claude", "cursor"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureStore(t)
			from, key := register(t, store, "cursor")
			to, toKey := register(t, store, kind)
			service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
				return Receipt{Status: "submitted"}
			}}
			msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "handle once"})
			if err != nil {
				t.Fatal(err)
			}
			incoming, err := store.QueryMessages(MessageQuery{Actor: to.ID, Capability: toKey, History: false, Limit: 0})
			if err != nil || len(incoming) != 1 {
				t.Fatalf("%+v %v", incoming, err)
			}
			if kind == "cursor" {
				if incoming[0].Body != msg.Body || incoming[0].BodyDeferred {
					t.Fatal("Cursor wake hint lost its only inbox read path")
				}
			} else if incoming[0].Body != "" || !incoming[0].BodyDeferred {
				t.Fatal("receipt read exposed a native message before its queued delivery")
			}
			outgoing, err := store.Messages(from.ID, key)
			if err != nil || len(outgoing) != 1 || outgoing[0].Body != msg.Body || outgoing[0].BodyDeferred {
				t.Fatalf("sender lost its receipt/body: %+v %v", outgoing, err)
			}
			persisted, err := store.message(msg.ID)
			if err != nil || persisted.Body != msg.Body || persisted.BodyDeferred {
				t.Fatalf("read-only projection changed durable contents: %+v %v", persisted, err)
			}
			if _, err := store.Ack(to.ID, toKey, msg.ID); err != nil {
				t.Fatal(err)
			}
			history, err := store.Messages(to.ID, toKey)
			if err != nil || len(history) != 1 || history[0].Body != msg.Body || history[0].BodyDeferred {
				t.Fatalf("acknowledged history lost content: %+v %v", history, err)
			}
		})
	}
}

func TestCompletedNativePromptGenerationFenceIsReadOnly(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "cursor")
	to, _ := register(t, store, "codex")
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		return NewReceipt(StatusSubmitted)
	}}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register(to.Kind, NativeID(to), to.CWD, nil); err != nil {
		t.Fatal(err)
	}
	if reason, err := store.completedNativePrompt(to.ID, Envelope(msg)); err != nil || !strings.Contains(reason, "previous registration") {
		t.Fatalf("missing generation fence: %q %v", reason, err)
	}
	stored, err := store.message(msg.ID)
	if err != nil || stored.AcknowledgedAt != "" || stored.Status != StatusSubmitted {
		t.Fatal("filtering changed the durable receipt", err)
	}
}
