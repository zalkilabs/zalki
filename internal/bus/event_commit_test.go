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
	if err := store.Update(session.ID, StateBusy, false); err != nil {
		t.Fatal(err)
	}
	metadata := eventState(t, store)
	if metadata.Count != 1 || metadata.Last != 1 || metadata.First != 1 {
		t.Fatalf("metadata = %+v", metadata)
	}
	raw := readCommitFile(t, filepath.Join(store.Dir, eventRecordPath(1)))
	var record eventRecord
	if err := decodeEventJSON(raw, &record); err != nil || record.Event.Session.State != StateBusy || record.Event.Session.Previous != StateReady {
		t.Fatalf("event = %+v, %v", record, err)
	}
	if err := store.Update(session.ID, StateBusy, false); err != nil {
		t.Fatal(err)
	}
	if got := eventState(t, store); got != metadata {
		t.Fatal("repeated busy update allocated another event")
	}
	marker, _, err := store.readStateFormats()
	if err != nil || marker.Formats["commits"] != 0 || marker.Formats["events"] != 0 {
		t.Fatalf("fences = %+v, %v", marker, err)
	}
}

func testCoveredUpdate(t *testing.T, store Store, id string, writer commitWriter) {
	t.Helper()
	if err := store.Update(id, StateReady, false); err != nil {
		t.Fatal(err)
	}
	err := store.locked(func() error {
		next, err := store.session(id)
		if err != nil {
			return err
		}
		next.State, next.UpdatedAt = StateBusy, model.Timestamp()
		return persistSessionUpdateWith(store, next, StateReady, writer)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEventRetentionNeverBlocksHealthyFullLog(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	writer := commitWriter{dir: store.Dir, limit: 2}
	for i := uint64(1); i <= 10; i++ {
		testCoveredUpdate(t, store, session.ID, writer)
		metadata := eventState(t, store)
		if metadata.Last != i || metadata.Count > 2 || metadata.Bytes > 2*maxEventBytes {
			t.Fatalf("unbounded metadata = %+v", metadata)
		}
		entries, err := os.ReadDir(filepath.Join(store.Dir, "events", "records"))
		if err != nil || uint64(len(entries)) != metadata.Count {
			t.Fatalf("record count = %d, %v", len(entries), err)
		}
	}
	metadata := eventState(t, store)
	if metadata.First != 9 || metadata.Count != 2 {
		t.Fatalf("low watermark did not advance: %+v", metadata)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, eventRecordPath(8))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old record remains: %v", err)
	}
}

func stagedLiveCommit(t *testing.T) (Store, Session, journal.Batch) {
	t.Helper()
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	path, err := store.sessionPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := readCommitFile(t, path)
	next := session
	next.State, next.UpdatedAt = StateBusy, model.Timestamp()
	event := model.Event{ID: newID(), Type: "session.state_changed", OccurredAt: next.UpdatedAt, TransactionID: newID(),
		Session: model.EventSession{ID: next.ID, Generation: next.Generation, Previous: StateReady, State: StateBusy}}
	batch, err := (commitWriter{dir: store.Dir}).prepare(before, next, event, nil)
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
	} else if err := store.Update(session.ID, StateApproval, false); err != nil {
		t.Fatal(err)
	}
	if got := eventState(t, store); got.Count != 1 {
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
	if err := store.Update(session.ID, StateApproval, false); err == nil {
		t.Fatal("write bypassed malformed intent")
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("malformed intent changed state")
	}
}

func TestEventPendingIntentUnderFormatFenceRefusesReadCallbacks(t *testing.T) {
	store, _, _ := stagedLiveCommit(t)
	futureStateMarker(t, store)
	before := journalSnapshot(t, store)
	called := false
	err := store.readLocked(func() error { called = true; return nil })
	if ErrorCodeOf(err) != CodeUnsupportedSchema || called {
		t.Fatalf("fenced pending intent permitted read callback: called=%v, err=%v", called, err)
	}
	if _, err := store.List(); ErrorCodeOf(err) != CodeUnsupportedSchema {
		t.Fatalf("public read bypassed fenced pending intent: %v", err)
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("fenced read recovered or changed state")
	}
	if err := os.Remove(filepath.Join(store.Dir, "commits/pending.json")); err != nil {
		t.Fatal(err)
	}
	before = journalSnapshot(t, store)
	if err := store.readLocked(func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("fence without pending intent changed legacy read policy: called=%v, err=%v", called, err)
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("fenced read without pending intent changed state")
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
	if err := store.Update(session.ID, StateBusy, false); err != nil {
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
	if err := store.Update(session.ID, StateBusy, false); err != nil {
		t.Fatalf("retry failed to finish directory setup: %v", err)
	}
}

func TestEventUpdatePreservesLegacySessionFormatDeclaration(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	if err := os.Remove(filepath.Join(store.Dir, stateFormatFile)); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(session.ID, StateBusy, false); err != nil {
		t.Fatal(err)
	}
	marker, _, err := store.readStateFormats()
	if err != nil || marker.Formats["sessions"] != recordSchema || len(marker.Formats) != 1 {
		t.Fatalf("existing session declaration changed: %+v, %v", marker, err)
	}
}
