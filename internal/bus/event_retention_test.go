package bus

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func retainedEventFixture(t *testing.T) (Store, Session, commitWriter) {
	t.Helper()
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	writer := commitWriter{dir: store.Dir, limit: 2}
	for range 2 {
		testCoveredUpdate(t, store, session.ID, writer)
	}
	setEventTestState(t, store, session.ID, model.StateReady, writer)
	return store, session, writer
}

func prepareRetainedCommit(t *testing.T, store Store, session Session, writer commitWriter) journal.Batch {
	t.Helper()
	path, err := store.sessionPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	prior := readCommitFile(t, path)
	next, err := store.session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	next.State, next.UpdatedAt = model.StateBusy, model.Timestamp()
	event := model.Event{ID: newID(), Type: "session.state_changed", OccurredAt: next.UpdatedAt, TransactionID: newID(),
		Session: model.EventSession{ID: next.ID, Generation: next.Generation, Previous: model.StateReady, State: model.StateBusy}}
	batch, err := writer.prepare(prior, next, event, nil)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func TestEventRetentionRecoversEveryMaterializationBoundary(t *testing.T) {
	for completed := range 5 {
		t.Run(string(rune('0'+completed)), func(t *testing.T) {
			store, session, writer := retainedEventFixture(t)
			batch := prepareRetainedCommit(t, store, session, writer)
			stageCommit(t, commitFixture{writer: writer}, batch)
			materializeRetentionCut(t, store, batch, completed)
			if _, err := store.List(); err != nil {
				t.Fatalf("public read did not recover retention: %v", err)
			}
			metadata := eventState(t, store)
			var expected eventMetadata
			if err := decodeEventJSON(batch.Puts[2].Data, &expected); err != nil {
				t.Fatal(err)
			}
			if metadata != expected {
				t.Fatalf("recovered retention = %+v", metadata)
			}
			got := readCommitFile(t, filepath.Join(store.Dir, batch.Puts[1].Path))
			if !bytes.Equal(got, batch.Puts[1].Data) {
				t.Fatal("recovery changed latest event")
			}
			if _, err := os.Stat(filepath.Join(store.Dir, batch.Deletes[0].Path)); !os.IsNotExist(err) {
				t.Fatalf("old record remains: %v", err)
			}
		})
	}
}

func materializeRetentionCut(t *testing.T, store Store, batch journal.Batch, completed int) {
	t.Helper()
	puts := min(completed, len(batch.Puts))
	for _, put := range batch.Puts[:puts] {
		if err := journal.WriteAtomic(filepath.Join(store.Dir, put.Path), put.Data); err != nil {
			t.Fatal(err)
		}
	}
	if completed > len(batch.Puts) {
		if err := os.Remove(filepath.Join(store.Dir, batch.Deletes[0].Path)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEventCorruptOrSymlinkOldestRefusesBeforeStateWrite(t *testing.T) {
	for _, fault := range []string{"corrupt", "symlink"} {
		t.Run(fault, func(t *testing.T) {
			assertInvalidOldestDoesNotWrite(t, fault)
		})
	}
}

func assertInvalidOldestDoesNotWrite(t *testing.T, fault string) {
	t.Helper()
	store, session, writer := retainedEventFixture(t)
	metadata := eventState(t, store)
	oldest := filepath.Join(store.Dir, eventRecordPath(metadata.First))
	if err := os.Remove(oldest); err != nil {
		t.Fatal(err)
	}
	if fault == "symlink" {
		if err := os.Symlink(filepath.Join(store.Dir, eventRecordPath(metadata.First+1)), oldest); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(oldest, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	before := journalSnapshot(t, store)
	err := store.locked(func() error {
		next, err := store.session(session.ID)
		if err != nil {
			return err
		}
		next.State, next.UpdatedAt = model.StateBusy, model.Timestamp()
		return saveSessionWith(store, next, writer)
	})
	if err == nil {
		t.Fatal("invalid oldest record accepted")
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("failed retention changed journal")
	}

}
