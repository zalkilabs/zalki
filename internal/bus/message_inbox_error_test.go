package bus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// An unreadable record no longer fails the list: it is skipped and counted, and
// the readable view keeps its authenticated content.
func TestMessageScanSkipKeepsTheView(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, _ := registerPair(t, store, "codex", "cursor")
	const first = "00000000-0000-4000-8000-000000000001"
	msg := model.Message{ID: first, From: from.ID, To: to.ID, Body: "fixture body", Status: model.StatusSubmitted, TargetGeneration: to.Generation}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(store.Dir, "messages", "00000000-0000-4000-8000-000000000002.json")
	if err := os.WriteFile(bad, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ReadMessages(MessageQuery{Actor: from.ID, Capability: key, Sent: true, IncludeBody: true, Preview: true})
	if err != nil || batch.More || len(batch.Messages) != 1 || batch.Unreadable != 1 {
		t.Fatalf("skip lost the readable result: count=%d more=%v unreadable=%d err=%v", len(batch.Messages), batch.More, batch.Unreadable, err)
	}
	view := batch.Messages[0]
	if view.ID != first || view.Body != msg.Body || view.BodyPreview != msg.Body {
		t.Fatal("the readable result lost its authenticated view")
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
