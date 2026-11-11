package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func eventState(t *testing.T, store Store) eventMetadata {
	t.Helper()
	metadata, _, err := (commitWriter{dir: store.Dir}).readMetadata()
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func TestEventUpdatePublishesOneCoveredTransition(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	metadata := eventState(t, store)
	if metadata.Count != 2 || metadata.Last != 2 || metadata.First != 1 {
		t.Fatalf("metadata = %+v", metadata)
	}
	raw := readCommitFile(t, filepath.Join(store.Dir, eventRecordPath(2)))
	var record eventRecord
	if err := decodeEventJSON(raw, &record); err != nil || record.Event.Session.State != model.StateBusy || record.Event.Session.Previous != model.StateReady {
		t.Fatalf("event = %+v, %v", record, err)
	}
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	if got := eventState(t, store); got != metadata {
		t.Fatal("repeated busy update allocated another event")
	}
}

func setEventTestState(t *testing.T, store Store, id string, state model.SessionState, writer commitWriter) {
	t.Helper()
	if err := store.locked(func() error {
		next, err := store.session(id)
		if err != nil {
			return err
		}
		next.State, next.UpdatedAt = state, model.Timestamp()
		return saveSessionWith(store, next, writer)
	}); err != nil {
		t.Fatal(err)
	}
}

func testCoveredUpdate(t *testing.T, store Store, id string, writer commitWriter) {
	t.Helper()
	setEventTestState(t, store, id, model.StateReady, writer)
	setEventTestState(t, store, id, model.StateBusy, writer)
}

func TestEventRetentionNeverBlocksHealthyFullLog(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	writer := commitWriter{dir: store.Dir, limit: 2}
	for i := uint64(1); i <= 10; i++ {
		testCoveredUpdate(t, store, session.ID, writer)
		metadata := eventState(t, store)
		if metadata.Last != 2*i || metadata.Count > 2 || metadata.Bytes > 2*maxEventBytes {
			t.Fatalf("unbounded metadata = %+v", metadata)
		}
		entries, err := os.ReadDir(filepath.Join(store.Dir, "events", "records"))
		if err != nil || uint64(len(entries)) != metadata.Count {
			t.Fatalf("record count = %d, %v", len(entries), err)
		}
	}
	metadata := eventState(t, store)
	if metadata.First != 19 || metadata.Count != 2 {
		t.Fatalf("low watermark did not advance: %+v", metadata)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, eventRecordPath(18))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old record remains: %v", err)
	}
}

func stagedLiveCommit(t *testing.T) (Store, model.Session, journal.Batch) {
	t.Helper()
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	path, err := store.sessionPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := readCommitFile(t, path)
	next := session
	next.State, next.UpdatedAt = model.StateBusy, model.Timestamp()
	event := model.Event{ID: newID(), Type: "session.state_changed", OccurredAt: next.UpdatedAt, TransactionID: newID(),
		Session: model.EventSession{ID: next.ID, Generation: next.Generation, Previous: model.StateReady, State: model.StateBusy}}
	batch, err := (commitWriter{dir: store.Dir}).prepare(before, next, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureEventDirectories(store, orderEventDirectory, false); err != nil {
		t.Fatal(err)
	}
	stageCommit(t, commitFixture{writer: commitWriter{dir: store.Dir}}, batch)
	return store, session, batch
}

func TestEventRecoveryPrecedesReadAndLegacyWrite(t *testing.T) {
	for _, operation := range []string{"read", "legacy write"} {
		for applied := range 4 {
			t.Run(operation+string(rune('0'+applied)), func(t *testing.T) {
				assertEventRecoveryCase(t, operation, applied)
			})
		}
	}
}

func assertEventRecoveryCase(t *testing.T, operation string, applied int) {
	t.Helper()
	store, session, batch := stagedLiveCommit(t)
	for _, put := range batch.Puts[:applied] {
		if err := journal.WriteAtomic(filepath.Join(store.Dir, put.Path), put.Data); err != nil {
			t.Fatal(err)
		}
	}
	if operation == "read" {
		if _, err := store.List(); err != nil {
			t.Fatal(err)
		}
	} else if err := store.Update(session.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	expectedCount := uint64(2)
	if operation != "read" {
		expectedCount++
	}
	if got := eventState(t, store); got.Count != expectedCount {
		t.Fatalf("recovery not complete: %+v", got)
	}
	got := readCommitFile(t, filepath.Join(store.Dir, batch.Puts[1].Path))
	if !bytes.Equal(got, batch.Puts[1].Data) {
		t.Fatal("recovery changed saved event ID/bytes")
	}
}

func TestEventMalformedIntentBlocksReadsAndWrites(t *testing.T) {
	store, session, _ := stagedLiveCommit(t)
	if err := os.WriteFile(filepath.Join(store.Dir, "commits/pending.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	before := journalSnapshot(t, store)
	if _, err := store.List(); err == nil {
		t.Fatal("read bypassed malformed intent")
	}
	if err := store.Update(session.ID, model.StateApproval, false); err == nil {
		t.Fatal("write bypassed malformed intent")
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("malformed intent changed state")
	}
}

func TestEventExactPriorWhitespaceAndUnknownFieldsSurvive(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	path, err := store.sessionPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.TrimSpace(readCommitFile(t, path))
	raw = append(bytes.Clone(raw[:len(raw)-1]), []byte(",\"custom\":{\"keep\":9}} \n\n")...)
	raw = append([]byte(" \n"), raw...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(readCommitFile(t, path), &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result["custom"], map[string]any{"keep": float64(9)}) || result["state"] != "busy" {
		t.Fatalf("merged record = %v", result)
	}
}

func TestEventDirectorySyncFailureLeavesStateAndFencesUntouched(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	before := journalSnapshot(t, store)
	stop := errors.New("directory sync failed")
	if err := ensureEventDirectories(store, func(string) error { return stop }, false); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("failed directory sync published a marker or changed state")
	}
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatalf("retry failed to finish directory setup: %v", err)
	}
}
