package bus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestMessageScanErrorKeepsPartialView(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, _ := registerPair(t, store, "codex", "cursor")
	const first = "00000000-0000-4000-8000-000000000001"
	msg := Message{ID: first, From: from.ID, To: to.ID, Body: "fixture body", Status: model.StatusSubmitted, TargetGeneration: to.Generation}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(store.Dir, "messages", "00000000-0000-4000-8000-000000000002.json")
	if err := os.WriteFile(bad, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ReadMessages(MessageQuery{Actor: from.ID, Capability: key, Sent: true, IncludeBody: true, Preview: true})
	if err == nil || batch.More || len(batch.Messages) != 1 {
		t.Fatalf("scan failure lost the prior partial result: count=%d more=%v err=%v", len(batch.Messages), batch.More, err)
	}
	view := batch.Messages[0]
	if view.ID != first || view.Body != msg.Body || view.BodyPreview != msg.Body {
		t.Fatal("partial result lost its authenticated view")
	}
}

func TestMessageCursorErrorKeepsNonNilEmptyView(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	batch, err := store.ReadMessages(MessageQuery{Actor: from.ID, Capability: key, Sent: true, Cursor: "00000000-0000-4000-8000-000000000003"})
	if err == nil || batch.Messages == nil || len(batch.Messages) != 0 || batch.More {
		t.Fatalf("cursor failure changed the empty result: nil=%v count=%d more=%v err=%v", batch.Messages == nil, len(batch.Messages), batch.More, err)
	}
}
