package bus

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	byteFixtureActor  = "cursor:00000000-0000-4000-8000-000000000071"
	byteFixtureGen    = "00000000-0000-4000-8000-000000000072"
	byteFixturePlugin = "00000000-0000-4000-8000-000000000073"
	byteFixtureInbox  = "00000000-0000-4000-8000-000000000075"
	byteFixtureNative = "00000000-0000-4000-8000-000000000076"
)

func TestPluginJournalEventAndRedoBytes(t *testing.T) {
	f := newCommitFixture(t)
	if err := os.Chmod(f.writer.dir, 0700); err != nil {
		t.Fatal(err)
	}
	first := f.event
	first.ID, first.TransactionID = byteFixtureNative, byteFixtureInbox
	first.Type, first.Session.Previous, first.Session.State = "session.registered", "", model.StateReady
	entry, err := journal.EncodeJSON(eventRecord{Epoch: byteFixturePlugin, Sequence: 1, Event: first}, maxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(f.writer.dir, eventRecordPath(1)), entry); err != nil {
		t.Fatal(err)
	}
	meta := eventMetadata{Epoch: byteFixturePlugin, First: 1, Last: 1, Count: 1, Bytes: uint64(len(entry))}
	if err := writeJSON(filepath.Join(f.writer.dir, eventMetaPath), meta); err != nil {
		t.Fatal(err)
	}
	stageCommit(t, f, preparedCommit(t, f))
	assertPluginJournalBytes(t, "redo intent", readCommitFile(t, filepath.Join(f.writer.dir, "commits", "pending.json")))
	store := Store{Dir: f.writer.dir, Registry: testRegistry()}
	if err := WithJournalRead(store, func(view JournalView) error {
		event, err := view.EventAt(byteFixturePlugin, 2)
		if err != nil || event != f.event {
			t.Fatal("view did not recover the fixed event", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertPluginJournalBytes(t, "event record", readCommitFile(t, filepath.Join(f.writer.dir, eventRecordPath(2))))
	assertPluginJournalBytes(t, "event metadata", readCommitFile(t, filepath.Join(f.writer.dir, eventMetaPath)))
}

func assertPluginJournalBytes(t *testing.T, name string, raw []byte) {
	t.Helper()
	// Captured with production files at 149740f before the journal boundary.
	want := map[string]string{
		"inbox message":  "2fcf250cc799f48e7e63a66ee3cea05e9210c572a57bae3f6971e372f81ca8cb",
		"native message": "6cb482bf5bacc7311c49c6ba2f304f1efd1aa94c9f13252c16beebb8611d5622",
		"redo intent":    "1a7911203480433b504c8831062eb66b37a0887bebd10904975d9562f08a7414",
		"event record":   "6cc6c4d35fcbf4c0e95deb13b643b371fa6ba81b9d88f27cba3a5e5cdd09c1d4",
		"event metadata": "bfd0377c967540dfc41e311bc4a01df29883984fc6788c8c3bae125873b4b60e",
	}[name]
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
		t.Fatalf("%s bytes changed: SHA256 %s, want %s", name, got, want)
	}
}

// Canonical message bytes stay with the actual bus record writer. The feature
// package seeds the same fixed input records only to exercise its audit paths.
func TestPluginJournalCanonicalMessageBytes(t *testing.T) {
	store := fixtureStore(t)
	now := time.Date(2026, 11, 10, 20, 0, 0, 123000000, time.UTC)
	at := now.Format(time.RFC3339Nano)
	native := model.Message{ID: byteFixtureNative, From: byteFixtureActor, To: byteFixtureActor, TargetGeneration: byteFixtureGen,
		Body: "native <>&", CreatedAt: at, Status: model.StatusSubmitted}
	inbox := model.Message{Schema: recordSchema, ID: byteFixtureInbox, From: byteFixtureActor, FromName: "Golden native", FromLabel: "Cursor",
		To: "plugin:" + byteFixturePlugin, TargetGeneration: byteFixturePlugin, Body: "inbox <>&", CreatedAt: at,
		Status: model.StatusSubmitted, Receipt: &model.Receipt{At: at, Status: model.StatusSubmitted}}
	if err := WithJournalWrite(store, func(JournalView) error {
		if err := store.saveMessage(native); err != nil {
			return err
		}
		return store.saveMessage(inbox)
	}); err != nil {
		t.Fatal(err)
	}
	assertPluginJournalBytes(t, "native message", readCommitFile(t, filepath.Join(store.Dir, "messages", byteFixtureNative+".json")))
	assertPluginJournalBytes(t, "inbox message", readCommitFile(t, filepath.Join(store.Dir, "messages", byteFixtureInbox+".json")))
}
