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
	if err != nil || got.State != StateBusy || got.ApprovalHeld != 1 {
		t.Fatalf("reply recovery overwrote/wedged state: %+v, %v", got, err)
	}
	if metadata := eventState(t, store); metadata.Count != 1 {
		t.Fatalf("legacy recovery emitted extra event: %+v", metadata)
	}
}

func seedLegacyHeldReply(t *testing.T, store Store, target Session) {
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
	parent := Message{Schema: 1, ID: newID(), From: target.ID, To: sender.ID, Body: "question",
		TargetGeneration: sender.Generation, Status: model.StatusSubmitted, CreatedAt: model.Timestamp()}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	reply := Message{Schema: 1, ID: replyID(sender.ID, parent.ID, "answer"), From: sender.ID, To: target.ID,
		Body: "answer", ReplyTo: parent.ID, TargetGeneration: target.Generation,
		Status: model.StatusAttempting, CreatedAt: model.Timestamp()}
	if err := reply.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(filepath.Join(store.Dir, "transactions")); err != nil {
		t.Fatal(err)
	}
	tx := replyTransaction{Schema: 1, ParentID: parent.ID, AcknowledgedAt: model.Timestamp(), Reply: reply}
	if err := store.writeRecord(filepath.Join(store.Dir, "transactions", reply.ID+".json"), tx, 1); err != nil {
		t.Fatal(err)
	}
}
