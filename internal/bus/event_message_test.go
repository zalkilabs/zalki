package bus

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func eventAt(t *testing.T, store Store, sequence uint64) model.Event {
	t.Helper()
	var record eventRecord
	if err := decodeEventJSON(readCommitFile(t, filepath.Join(store.Dir, eventRecordPath(sequence))), &record); err != nil {
		t.Fatal(err)
	}
	return record.Event
}

func newEventMessage(t *testing.T, store Store, body string) model.Message {
	t.Helper()
	from, _, to, _ := registerPair(t, store, "codex", "cursor")
	return model.Message{Schema: recordSchema, ID: newID(), From: from.ID, To: to.ID, Body: body,
		TargetGeneration: to.Generation, CreatedAt: model.Timestamp(), Status: model.StatusAttempting}
}

func saveLocked(t *testing.T, store Store, msg model.Message) {
	t.Helper()
	if err := store.locked(func() error { return store.saveMessage(msg) }); err != nil {
		t.Fatal(err)
	}
}

// Each change of status, hold or acknowledgement commits exactly one event;
// a rewrite that changes none of them commits none.
func TestMessageWritesEmitOneEventPerChange(t *testing.T) {
	store := fixtureStore(t)
	msg := newEventMessage(t, store, "body")
	base := eventState(t, store).Last
	saveLocked(t, store, msg)
	msg.Status = model.StatusTransportWritten
	saveLocked(t, store, msg)
	msg.Receipt = &model.Receipt{At: model.Timestamp(), Status: model.StatusTransportWritten}
	saveLocked(t, store, msg)
	msg.AcknowledgedAt = model.Timestamp()
	saveLocked(t, store, msg)
	if got := eventState(t, store).Last - base; got != 3 {
		t.Fatalf("events = %d, want 3 (create, transport_written, ack)", got)
	}
	want := []model.EventMessage{
		{Status: model.StatusAttempting},
		{Previous: model.StatusAttempting, Status: model.StatusTransportWritten},
		{Previous: model.StatusTransportWritten, Status: model.StatusTransportWritten, Acknowledged: true},
	}
	for i, w := range want {
		event := eventAt(t, store, base+uint64(i)+1)
		w.ID, w.From, w.To = msg.ID, msg.From, msg.To
		if event.Type != model.EventMessageStatusChanged || event.Message == nil || *event.Message != w || event.Session != (model.EventSession{}) {
			t.Fatalf("event %d = %+v %+v, want %+v", i, event, event.Message, w)
		}
	}
}

// A hold and its release are message events with the held flag.
func TestMessageHoldAndReleaseAreEvents(t *testing.T) {
	store := fixtureStore(t)
	msg := newEventMessage(t, store, "body")
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	saveLocked(t, store, msg)
	held := latestSessionEvent(t, store).Message
	if held == nil || !held.Held || held.Status != model.StatusApprovalBlocked {
		t.Fatalf("hold event = %+v", held)
	}
	if err := msg.ReleaseHeld(model.StatusAttempting); err != nil {
		t.Fatal(err)
	}
	saveLocked(t, store, msg)
	released := latestSessionEvent(t, store).Message
	if released == nil || released.Held || !released.PreviousHeld || released.Status != model.StatusAttempting {
		t.Fatalf("release event = %+v", released)
	}
}

// No event record holds message text or its size.
func TestMessageEventsHoldNoBody(t *testing.T) {
	store := fixtureStore(t)
	const body = "private-body-text-7f3a"
	msg := newEventMessage(t, store, body)
	saveLocked(t, store, msg)
	msg.Status = model.StatusTransportWritten
	saveLocked(t, store, msg)
	entries, err := os.ReadDir(filepath.Join(store.Dir, "events", "records"))
	if err != nil || len(entries) == 0 {
		t.Fatal(entries, err)
	}
	for _, entry := range entries {
		raw := readCommitFile(t, filepath.Join(store.Dir, "events", "records", entry.Name()))
		if bytes.Contains(raw, []byte(body)) || bytes.Contains(raw, []byte(`"body`)) {
			t.Fatalf("event %s holds the body: %s", entry.Name(), raw)
		}
	}
}

// A message batch cut after the message file recovers with one event and the
// exact message bytes; a second recovery changes nothing.
func TestMessageCommitRecoversWithOneEvent(t *testing.T) {
	store := fixtureStore(t)
	msg := newEventMessage(t, store, "body")
	writer := commitWriter{dir: store.Dir}
	event, err := messageStatusEvent(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := writer.prepareMessage(nil, msg, event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := journal.EncodeJSON(batch, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(store.Dir, "commits", "pending.json"), raw); err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(store.Dir, batch.Puts[0].Path), batch.Puts[0].Data); err != nil {
		t.Fatal(err)
	}
	before := eventState(t, store).Last
	for range 2 {
		if err := store.locked(func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if got := eventState(t, store).Last; got != before+1 {
		t.Fatalf("last event = %d, want %d", got, before+1)
	}
	if got := latestSessionEvent(t, store); got.ID != event.ID {
		t.Fatalf("recovered event = %+v, want %+v", got, event)
	}
	if got := readCommitFile(t, filepath.Join(store.Dir, batch.Puts[0].Path)); !bytes.Equal(got, batch.Puts[0].Data) {
		t.Fatal("recovery changed the message bytes")
	}
}

// Recovery refuses a batch whose event does not match the message bytes.
func TestMessageBatchValidationChecksTheMessage(t *testing.T) {
	store := fixtureStore(t)
	msg := newEventMessage(t, store, "body")
	writer := commitWriter{dir: store.Dir}
	event, err := messageStatusEvent(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := writer.prepareMessage(nil, msg, event)
	if err != nil {
		t.Fatal(err)
	}
	other := msg
	other.Status = model.StatusTransportWritten
	data, err := encodeStateJSON(other)
	if err != nil {
		t.Fatal(err)
	}
	batch.Puts[0] = journal.NewPut(batch.Puts[0].Path, nil, data)
	if err := writer.validate(batch); err == nil {
		t.Fatal("validate accepted a message that does not match its event")
	}
}

// A writer still refuses to overwrite an untrusted record when an event is due.
func TestMessageEventWriterKeepsTheRefusal(t *testing.T) {
	store := fixtureStore(t)
	msg := newEventMessage(t, store, "body")
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	before := []byte(`{"schema":2,"id":"` + msg.ID + `"}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	last := eventState(t, store).Last
	if err := store.locked(func() error { return store.saveMessage(msg) }); err == nil {
		t.Fatal("the writer overwrote an untrusted record")
	}
	if after := readCommitFile(t, path); !bytes.Equal(after, before) || eventState(t, store).Last != last {
		t.Fatal("a refused write changed the record or the events")
	}
}

// The open-message marker exists before the batch and is dropped after a
// closing write.
func TestMessageEventKeepsOpenMarkers(t *testing.T) {
	store := fixtureStore(t)
	msg := newEventMessage(t, store, "body")
	msg.Status = model.StatusPendingActivity
	saveLocked(t, store, msg)
	marker := filepath.Join(store.Dir, "open-messages", msg.ID)
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("open message has no marker: %v", err)
	}
	msg.Status, msg.AcknowledgedAt = model.StatusContextEmitted, model.Timestamp()
	saveLocked(t, store, msg)
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("closed message kept its marker: %v", err)
	}
}

// Session events carry the cause of the writer that changed the state.
func TestSessionEventsCarryTheirCause(t *testing.T) {
	cases := []struct {
		name  string
		run   func(Store, model.Session) error
		cause model.EventCause
	}{
		{"stop", func(s Store, v model.Session) error {
			if err := s.Update(v.ID, model.StateBusy, false); err != nil {
				return err
			}
			return (&Service{Store: s}).applyLifecycleState(v.ID, "Stop")
		}, model.CauseStop},
		{"interrupt", func(s Store, v model.Session) error {
			if err := s.Update(v.ID, model.StateBusy, false); err != nil {
				return err
			}
			return (&Service{Store: s}).applyLifecycleState(v.ID, "Interrupt")
		}, model.CauseInterrupt},
		{"permission", func(s Store, v model.Session) error {
			return (&Service{Store: s}).applyLifecycleState(v.ID, "PermissionRequest")
		}, model.CausePermission},
		{"session end", func(s Store, v model.Session) error {
			return (&Service{Store: s}).applyLifecycleState(v.ID, "SessionEnd")
		}, model.CauseSessionEnd},
		{"host update", func(s Store, v model.Session) error {
			return (hookRuntime{store: s, actor: v.ID, event: "PostToolUse"}).UpdateSession(v.ID, model.StateBusy, false)
		}, model.CauseTool},
		{"explicit cleanup", func(s Store, v model.Session) error {
			_, err := s.PruneRetired(false, []string{v.ID}, 0)
			return err
		}, model.CauseCleanup},
		{"no cause", func(s Store, v model.Session) error { return s.Update(v.ID, model.StateBusy, false) }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fixtureStore(t)
			session, _ := register(t, store, "cursor")
			if got := eventAt(t, store, 1).Session.Cause; got != model.CauseRegister {
				t.Fatalf("registration cause = %q", got)
			}
			if err := tc.run(store, session); err != nil {
				t.Fatal(err)
			}
			if got := latestSessionEvent(t, store).Session.Cause; got != tc.cause {
				t.Fatalf("cause = %q, want %q", got, tc.cause)
			}
		})
	}
}

// A reply is written only between its parent's parties, reversed. A reply_to
// that points at an unrelated message is refused before anything is written,
// so the feed can keep reply_to without a parent lookup.
func TestReplyToMustReverseTheParentParties(t *testing.T) {
	store := fixtureStore(t)
	parent := newEventMessage(t, store, "question")
	saveLocked(t, store, parent)
	unrelated := newEventMessage(t, store, "other")
	saveLocked(t, store, unrelated)
	reply := model.Message{Schema: recordSchema, ID: newID(), From: parent.To, To: parent.From, Body: "answer",
		ReplyTo: parent.ID, TargetGeneration: newID(), CreatedAt: model.Timestamp(), Status: model.StatusAttempting}
	forged := reply
	forged.ID, forged.ReplyTo = newID(), unrelated.ID
	last := eventState(t, store).Last
	if err := store.locked(func() error { return store.saveMessage(forged) }); err == nil {
		t.Fatal("a reply to an unrelated parent was written")
	}
	if _, err := os.Lstat(filepath.Join(store.Dir, "messages", forged.ID+".json")); !os.IsNotExist(err) || eventState(t, store).Last != last {
		t.Fatalf("a refused reply left a record or an event: %v", err)
	}
	saveLocked(t, store, reply)
	if got := latestSessionEvent(t, store).Message; got == nil || got.ReplyTo != parent.ID {
		t.Fatalf("reply event = %+v", got)
	}
}
