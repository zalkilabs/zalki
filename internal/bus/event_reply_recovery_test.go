package bus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestEventRecoveryRunsBeforeLegacyReplyChangesSession(t *testing.T) {
	store, target, _ := stagedLiveCommit(t)
	seedLegacyHeldReply(t, store, target)
	if _, err := store.List(); err != nil {
		t.Fatalf("mixed recovery failed: %v", err)
	}
	got, err := store.session(target.ID)
	if err != nil || got.State != model.StateBusy || got.ApprovalHeld != 1 {
		t.Fatalf("reply recovery overwrote/wedged state: %+v, %v", got, err)
	}
	// Event 2 is the staged session change; reply recovery then adds the held
	// reply (3) and the parent's acknowledgement (4).
	if metadata := eventState(t, store); metadata.Count != 4 {
		t.Fatalf("recovery events = %+v, want 4", metadata)
	}
	if event := eventAt(t, store, 2); event.Session.State != model.StateBusy {
		t.Fatalf("event 2 = %+v, want the staged session change", event)
	}
}

func seedLegacyHeldReply(t *testing.T, store Store, target model.Session) {
	t.Helper()
	// Registering would recover first; seed the unrelated sender in a separate
	// store, then copy only its record to arrange simultaneous recovery inputs.
	otherStore := fixtureStore(t)
	sender, _ := register(t, otherStore, "codex")
	source, _ := otherStore.sessionPath(sender.ID)
	destination, _ := store.sessionPath(sender.ID)
	if err := os.WriteFile(destination, readCommitFile(t, source), 0600); err != nil {
		t.Fatal(err)
	}
	parent := model.Message{Schema: 1, ID: newID(), From: target.ID, To: sender.ID, Body: "question",
		TargetGeneration: sender.Generation, Status: model.StatusSubmitted, CreatedAt: model.Timestamp()}
	// The parent predates the crash; a plain write keeps the staged intent.
	if err := store.writeRecord(filepath.Join(store.Dir, "messages", parent.ID+".json"), parent); err != nil {
		t.Fatal(err)
	}
	reply := model.Message{Schema: 1, ID: replyID(sender.ID, parent.ID, "answer"), From: sender.ID, To: target.ID,
		Body: "answer", ReplyTo: parent.ID, TargetGeneration: target.Generation,
		Status: model.StatusAttempting, CreatedAt: model.Timestamp()}
	if err := reply.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(filepath.Join(store.Dir, "transactions")); err != nil {
		t.Fatal(err)
	}
	tx := replyTransaction{Schema: 1, ParentID: parent.ID, AcknowledgedAt: model.Timestamp(), Reply: reply}
	if err := store.writeRecord(filepath.Join(store.Dir, "transactions", reply.ID+".json"), tx); err != nil {
		t.Fatal(err)
	}
}
