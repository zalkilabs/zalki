package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type commitFixture struct {
	writer   commitWriter
	previous json.RawMessage
	next     model.Session
	event    model.Event
}

func newCommitFixture(t *testing.T) commitFixture {
	t.Helper()
	dir := t.TempDir()
	for _, subdir := range []string{"sessions", "commits", "events/records"} {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	session := model.Session{Schema: 1, ID: "codex:00000000-0000-4000-8000-000000000003",
		Kind: "codex", NativeID: "00000000-0000-4000-8000-000000000003",
		Generation: "00000000-0000-4000-8000-000000000004", State: model.StateReady,
		CWD: "/private/workspace", KeyHash: "private-key-hash", Task: "private task"}
	raw, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	// Include extension bytes that a typed Session alone would lose.
	raw = append(raw[:len(raw)-1], []byte(",\"extension\":{\"keep\":true}}\n")...)
	event := model.Event{ID: "00000000-0000-4000-8000-000000000001",
		Type: "session.state_changed", OccurredAt: "2026-11-07T11:00:00Z",
		TransactionID: "00000000-0000-4000-8000-000000000002",
		Session:       model.EventSession{ID: session.ID, Generation: session.Generation, Previous: model.StateReady, State: model.StateBusy}}
	if err := os.WriteFile(filepath.Join(dir, eventSubjectPath(event)), raw, 0600); err != nil {
		t.Fatal(err)
	}
	session.State = model.StateBusy
	return commitFixture{commitWriter{dir: dir}, raw, session, event}
}

func readCommitFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDormantCommitPublishesMatchingStateAndMetadata(t *testing.T) {
	f := newCommitFixture(t)
	if err := f.writer.commit(f.previous, f.next, f.event); err != nil {
		t.Fatal(err)
	}
	raw := readCommitFile(t, filepath.Join(f.writer.dir, eventSubjectPath(f.event)))
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if _, kept := state["extension"]; state["state"] != "busy" || kept {
		t.Fatalf("state/extension = %v, want busy and no unknown field", state)
	}
	metadata := readCommitFile(t, filepath.Join(f.writer.dir, eventRecordPath(1)))
	var got eventRecord
	if err := json.Unmarshal(metadata, &got); err != nil || got.Event != f.event {
		t.Fatalf("event = %+v, %v", got, err)
	}
	for _, secret := range []string{"private-key-hash", "private task", "/private/workspace", "key_hash", "cwd"} {
		if bytes.Contains(metadata, []byte(secret)) {
			t.Fatalf("metadata leaked %q", secret)
		}
	}
	if err := f.writer.recover(); err != nil {
		t.Fatalf("completed recovery: %v", err)
	}
}

func preparedCommit(t *testing.T, f commitFixture) journal.Batch {
	t.Helper()
	batch, err := f.writer.prepare(f.previous, f.next, f.event)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func stageCommit(t *testing.T, f commitFixture, batch journal.Batch) {
	t.Helper()
	raw, err := journal.EncodeJSON(batch, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(f.writer.dir, "commits", "pending.json"), raw); err != nil {
		t.Fatal(err)
	}
}

func TestDormantCommitRecoversWithoutChangingEventIDOrBytes(t *testing.T) {
	f := newCommitFixture(t)
	batch := preparedCommit(t, f)
	stageCommit(t, f, batch)
	// Model a crash after the state file, but before event materialization.
	if err := journal.WriteAtomic(filepath.Join(f.writer.dir, batch.Puts[0].Path), batch.Puts[0].Data); err != nil {
		t.Fatal(err)
	}
	if err := f.writer.recover(); err != nil {
		t.Fatal(err)
	}
	got := readCommitFile(t, filepath.Join(f.writer.dir, batch.Puts[1].Path))
	if !bytes.Equal(got, batch.Puts[1].Data) {
		t.Fatal("recovery changed event bytes")
	}
	stageCommit(t, f, batch)
	if err := f.writer.recover(); err != nil {
		t.Fatalf("repeated recovery: %v", err)
	}
	if after := readCommitFile(t, filepath.Join(f.writer.dir, batch.Puts[1].Path)); !bytes.Equal(after, got) {
		t.Fatal("repeated recovery changed event")
	}
}

func TestDormantCommitRejectsMismatchedEventBeforeWriting(t *testing.T) {
	for _, mismatch := range []string{"before", "after", "generation", "future schema"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newCommitFixture(t)
			switch mismatch {
			case "before":
				f.event.Session.Previous = model.StateApproval
			case "after":
				f.next.State = model.StateApproval
			case "generation":
				f.next.Generation = newID()
			case "future schema":
				f.next.Schema = 99
			}
			if err := f.writer.commit(f.previous, f.next, f.event); err == nil {
				t.Fatal("mismatched state/event accepted")
			}
			got := readCommitFile(t, filepath.Join(f.writer.dir, eventSubjectPath(f.event)))
			if !bytes.Equal(got, f.previous) {
				t.Fatal("state changed after rejection")
			}
		})
	}
}

func TestDormantRecoveryRejectsForeignFamilyBeforeAnyWrite(t *testing.T) {
	f := newCommitFixture(t)
	batch := preparedCommit(t, f)
	batch.Puts[0].Path = "sessions/foreign.json"
	stageCommit(t, f, batch)
	if err := f.writer.recover(); !errors.Is(err, journal.ErrInvalidBatch) {
		t.Fatalf("foreign target = %v", err)
	}
	got := readCommitFile(t, filepath.Join(f.writer.dir, eventSubjectPath(f.event)))
	if !bytes.Equal(got, f.previous) {
		t.Fatal("state changed before validation")
	}
}

func TestSessionLifecycleWritersActivateOutbox(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	if err := store.Update(session.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	if got := eventState(t, store); got.Count != 2 {
		t.Fatalf("registration and approval facts = %+v", got)
	}
}

func TestDormantRecoveryRejectsPrivateEventFields(t *testing.T) {
	f := newCommitFixture(t)
	batch := preparedCommit(t, f)
	raw := bytes.TrimSpace(batch.Puts[1].Data)
	raw = append(bytes.Clone(raw[:len(raw)-1]), []byte(",\"body\":\"private\"}\n")...)
	batch.Puts[1] = journal.NewPut(batch.Puts[1].Path, nil, raw)
	stageCommit(t, f, batch)
	if err := f.writer.recover(); err == nil {
		t.Fatal("private event field accepted")
	}
	got := readCommitFile(t, filepath.Join(f.writer.dir, eventSubjectPath(f.event)))
	if !bytes.Equal(got, f.previous) {
		t.Fatal("state changed before metadata validation")
	}
}
