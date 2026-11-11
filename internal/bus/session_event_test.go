package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func latestSessionEvent(t *testing.T, store Store) model.Event {
	t.Helper()
	meta := eventState(t, store)
	var record eventRecord
	if err := decodeEventJSON(readCommitFile(t, filepath.Join(store.Dir, eventRecordPath(meta.Last))), &record); err != nil {
		t.Fatal(err)
	}
	return record.Event
}

func TestSessionRegistrationProjection(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "cursor")
	event := latestSessionEvent(t, store)
	if event.Type != "session.registered" || event.Session.ID != session.ID || event.Session.Generation != session.Generation || event.Session.Previous != "" {
		t.Fatalf("initial registration = %+v", event)
	}
	next, _, err := store.Register("cursor", session.NativeID, session.CWD, nil)
	if err != nil {
		t.Fatal(err)
	}
	event = latestSessionEvent(t, store)
	if event.Type != "session.registered" || event.Session.Generation != next.Generation || next.Generation == session.Generation || eventState(t, store).Last != 2 {
		t.Fatalf("replacement = %+v", event)
	}
	if _, _, err := store.register("cursor", next.NativeID, next.CWD, nil, true); err != nil {
		t.Fatal(err)
	}
	if eventState(t, store).Last != 2 {
		t.Fatal("compaction generated a lifecycle fact")
	}
}

func TestSessionLifecycleProducerEvents(t *testing.T) {
	cases := []struct {
		name    string
		run     func(Store, model.Session) error
		state   model.SessionState
		retired bool
	}{
		{"Update approval", func(s Store, v model.Session) error { return s.Update(v.ID, model.StateApproval, false) }, model.StateApproval, false},
		{"Update busy", func(s Store, v model.Session) error { return s.Update(v.ID, model.StateBusy, false) }, model.StateBusy, false},
		{"hook permission", func(s Store, v model.Session) error {
			return (&Service{Store: s}).applyLifecycleState(v.ID, "PermissionRequest")
		}, model.StateApproval, false},
		{"host update", func(s Store, v model.Session) error {
			return (hookRuntime{store: s, actor: v.ID}).UpdateSession(v.ID, model.StateBusy, false)
		}, model.StateBusy, false},
		{"adapter prompt", func(s Store, v model.Session) error {
			return cursorHost(s, v.ID, "UserPromptSubmit").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
				return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate}, nil
			})
		}, model.StateBusy, false},
		{"retirement", func(s Store, v model.Session) error { return s.Update(v.ID, model.StateRetired, true) }, model.StateRetired, true},
		{"prune retirement", func(s Store, v model.Session) error { _, err := s.PruneRetired(false, []string{v.ID}, 0); return err }, model.StateRetired, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fixtureStore(t)
			session, _ := register(t, store, "cursor")
			if err := tc.run(store, session); err != nil {
				t.Fatal(err)
			}
			event := latestSessionEvent(t, store)
			if event.Type != "session.state_changed" || event.Session.Previous != model.StateReady || event.Session.State != tc.state || event.Session.Retired != tc.retired || eventState(t, store).Last != 2 {
				t.Fatalf("lifecycle event = %+v", event)
			}
		})
	}
}

func TestSessionReadyAndReactivationEvents(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "cursor")
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	if err := cursorHost(store, session.ID, "Stop").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{Transition: agent.TransitionReady}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if e := latestSessionEvent(t, store); e.Session.Previous != model.StateBusy || e.Session.State != model.StateReady {
		t.Fatal(e)
	}
	if err := store.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if err := cursorHost(store, session.ID, "UserPromptSubmit").ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if e := latestSessionEvent(t, store); e.Session.Previous != model.StateRetired || !e.Session.PreviousRetired || e.Session.Retired || e.Session.State != model.StateBusy {
		t.Fatal(e)
	}
}

func TestSessionRetiredFlagWithoutStateChangeProducesFact(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "cursor")
	before := eventState(t, store)
	session.Retired = true
	if err := store.locked(func() error { return store.saveSession(session) }); err != nil {
		t.Fatal(err)
	}
	event := latestSessionEvent(t, store)
	if event.Type != "session.state_changed" || event.Session.Previous != model.StateReady || event.Session.State != model.StateReady || event.Session.PreviousRetired || !event.Session.Retired {
		t.Fatalf("retirement-only fact = %+v", event)
	}
	if after := eventState(t, store); after.Count != before.Count+1 || after.Last != before.Last+1 {
		t.Fatalf("retirement-only fact count = %+v", after)
	}
}

func TestSessionRecoveryRefusesRetiredProjectionMismatch(t *testing.T) {
	store, session, batch := stagedLiveCommit(t)
	path, err := store.sessionPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	prior := readCommitFile(t, path)
	var next model.Session
	if err := json.Unmarshal(batch.Puts[0].Data, &next); err != nil {
		t.Fatal(err)
	}
	next.Retired = true
	data, err := encodeStateJSON(next)
	if err != nil {
		t.Fatal(err)
	}
	batch.Puts[0] = journal.NewPut(batch.Puts[0].Path, prior, data)
	stageCommit(t, commitFixture{writer: commitWriter{dir: store.Dir}}, batch)
	before := journalSnapshot(t, store)
	if _, err := store.List(); err == nil || !strings.Contains(err.Error(), "event does not match resulting session") {
		t.Fatalf("retirement projection mismatch = %v", err)
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("retirement projection mismatch changed the journal")
	}
}

func TestSessionPrivateProducersEmitNoLifecycleFact(t *testing.T) {
	store := fixtureStore(t)
	session, key := register(t, store, "cursor")
	before := eventState(t, store)
	if _, err := (&Service{Store: store}).SetTask(session.ID, key, "private task"); err != nil {
		t.Fatal(err)
	}
	if err := store.hookName(session.ID, "private-name"); err != nil {
		t.Fatal(err)
	}
	if err := store.rememberOwner(session.ID, "cursor", true); err != nil {
		t.Fatal(err)
	}
	if err := store.locked(func() error { return store.rememberApprovalHold(session.ID, 1) }); err != nil {
		t.Fatal(err)
	}
	if err := store.locked(func() error { return store.clearApprovalHeld(session.ID) }); err != nil {
		t.Fatal(err)
	}
	lease := armedLease(t, store, session.ID, key)
	if err := lease.Confirm(func(model.SessionView) (json.RawMessage, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	lease.Close(nil)
	if _, err := cursorHost(store, session.ID, "PostToolUse").ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
		return agent.RecoveryDecision{Recover: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := eventState(t, store); got != before {
		t.Fatalf("private writes created facts: %+v", got)
	}
}

func TestPrivateSessionAtomicSaveAndBypassRefusal(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	before := eventState(t, store)
	path, _ := store.sessionPath(session.ID)
	session.Task = "private"
	if err := store.locked(func() error { return store.saveSession(session) }); err != nil {
		t.Fatal(err)
	}
	saved, err := store.session(session.ID)
	if err != nil || saved.Task != "private" || eventState(t, store) != before {
		t.Fatalf("private atomic save = %+v,%v", saved, err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "commits", "pending.json")); !os.IsNotExist(err) {
		t.Fatal("private save left an intent", err)
	}
	if err := store.writeRecord(path, session); err == nil || !strings.Contains(err.Error(), "commit path") {
		t.Fatal("raw session bypass accepted", err)
	}
}

func TestPrivateSessionAtomicSaveRefusesStalePrior(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	path, _ := store.sessionPath(session.ID)
	prior := readCommitFile(t, path)
	session.Task = "requested"
	record, err := prepareSessionRecord(store, session)
	if err != nil {
		t.Fatal(err)
	}
	intervening := session
	intervening.Task = "intervening"
	data, err := encodeStateJSON(intervening)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(path, data); err != nil {
		t.Fatal(err)
	}
	if err := store.locked(func() error { return savePrivateSessionRecord(record, prior) }); !errors.Is(err, journal.ErrBatchConflict) {
		t.Fatalf("stale prior = %v", err)
	}
	if !bytes.Equal(readCommitFile(t, path), data) {
		t.Fatal("stale private save overwrote intervening data")
	}
}

func TestSessionInitialRegistrationRecoveryCuts(t *testing.T) {
	for applied := range 4 {
		t.Run(string(rune('0'+applied)), func(t *testing.T) { assertInitialSessionCut(t, applied) })
	}
}

func assertInitialSessionCut(t *testing.T, applied int) {
	t.Helper()
	store := fixtureStore(t)
	native := newID()
	next := model.Session{Schema: recordSchema, ID: "cursor:" + native, NativeID: native, Kind: "cursor", Generation: newID(), State: model.StateReady, CWD: "/workspace", KeyHash: "private test key", UpdatedAt: model.Timestamp()}
	batch := stageInitialSessionCut(t, store, next, applied)
	sessions, err := store.List()
	if err != nil || len(sessions) != 1 || sessions[0].ID != next.ID {
		t.Fatalf("initial registration recovery = %+v, %v", sessions, err)
	}
	if !bytes.Equal(readCommitFile(t, filepath.Join(store.Dir, batch.Puts[1].Path)), batch.Puts[1].Data) {
		t.Fatal("registration replay changed the event")
	}
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}
	if e := latestSessionEvent(t, store); e.Type != "session.registered" || e.Session.Previous != "" || eventState(t, store).Count != 1 {
		t.Fatal(e)
	}
}

func stageInitialSessionCut(t *testing.T, store Store, next model.Session, applied int) journal.Batch {
	t.Helper()
	var batch journal.Batch
	if err := store.locked(func() error {
		record, err := prepareSessionRecord(store, next)
		if err != nil {
			return err
		}
		event, err := sessionEvent(nil, record.value)
		if err != nil {
			return err
		}
		batch, err = (commitWriter{dir: store.Dir}).prepare(nil, record.value, event)
		if err != nil {
			return err
		}
		if err := ensureEventDirectories(store, orderEventDirectory, false); err != nil {
			return err
		}
		stageCommit(t, commitFixture{writer: commitWriter{dir: store.Dir}}, batch)
		for _, put := range batch.Puts[:applied] {
			if err := journal.WriteAtomic(filepath.Join(store.Dir, put.Path), put.Data); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return batch
}

func TestOnePutSessionIntentRefusesCallbacks(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	path, _ := store.sessionPath(session.ID)
	previous := readCommitFile(t, path)
	session.Task = "private"
	data, err := encodeStateJSON(session)
	if err != nil {
		t.Fatal(err)
	}
	batch := journal.Batch{ID: newID(), Puts: []journal.Put{journal.NewPut(eventSessionPath(model.Event{Session: model.EventSession{ID: session.ID}}), previous, data)}}
	stageCommit(t, commitFixture{writer: commitWriter{dir: store.Dir}}, batch)
	if _, err := store.List(); err == nil {
		t.Fatal("one-put session intent accepted")
	}
	if !bytes.Equal(readCommitFile(t, path), previous) {
		t.Fatal("unsupported intent changed the session")
	}
}
