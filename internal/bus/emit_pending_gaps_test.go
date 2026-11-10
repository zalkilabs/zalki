package bus

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// These tests close gaps found in Store.EmitPending (message_inbox.go). They
// pin today's behavior; odd results are characterized, not endorsed.
//
// EmitPending does this, in this order, under the journal lock:
//  1. Load the session. A missing session is an error.
//  2. The adapter must deliver through hooks: "this adapter does not support
//     hook delivery". This is checked before anything else is read or settled.
//  3. Resolve the approval queue (resolveApprovalQueue). If the target is
//     unreachable (retired, or its owner has exited) this returns at once with
//     an empty context and NO error. Queued approvals of an unreachable target
//     are settled as not sent on the way.
//  4. Read every message. An unreadable record is skipped and reported once
//     (message_unreadable_hook_test.go). A message is pending when it is for
//     this session and generation, in a hook-pending status, and not
//     acknowledged.
//  5. Sort the pending messages by CreatedAt.
//  6. For each message in that order: stop at the 6th message, or when adding
//     it would push the context over 24 KiB (exactly 24 KiB still fits). The
//     rest stay pending for a later call; a smaller later message is not
//     skipped ahead. Otherwise mark it context-emitted, save it, and add its
//     envelope plus a blank line to the context.

const (
	emitMessageCap  = 5
	emitContextCap  = 24 * 1024
	emitHookPending = model.StatusPendingActivity
)

// emitFixture is a cursor target (it delivers through hooks) and a sender.
type emitFixture struct {
	store  Store
	target model.Session
	sender model.Session
	key    string
}

func newEmitFixture(t *testing.T) emitFixture {
	t.Helper()
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "codex", "cursor")
	return emitFixture{store: store, target: target, sender: sender, key: key}
}

// queue stores a hook-pending message with the given body, created at the
// given time, and returns it.
func (f emitFixture) queue(t *testing.T, body string, created time.Time) model.Message {
	t.Helper()
	msg, _, _, err := f.store.ReserveMessage(SendRequest{Actor: f.sender.ID, Capability: f.key, To: f.target.ID, MessageID: newID(), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Finish(msg.ID, model.Receipt{Status: emitHookPending}); err != nil {
		t.Fatal(err)
	}
	f.rewrite(t, msg.ID, func(m *model.Message) { m.CreatedAt = created.UTC().Format(time.RFC3339Nano) })
	return f.read(t, msg.ID)
}

func (f emitFixture) read(t *testing.T, id string) model.Message {
	t.Helper()
	msg, err := f.store.message(id)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func (f emitFixture) rewrite(t *testing.T, id string, change func(*model.Message)) {
	t.Helper()
	msg := f.read(t, id)
	change(&msg)
	if err := f.store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
}

// emitted lists which messages appear in the context, in order of appearance.
func emitted(t *testing.T, context string, all []model.Message) []string {
	t.Helper()
	type hit struct {
		at int
		id string
	}
	var found []hit
	for _, msg := range all {
		if at := strings.Index(context, msg.ID); at >= 0 {
			found = append(found, hit{at, msg.ID})
		}
	}
	slices.SortFunc(found, func(x, y hit) int { return x.at - y.at })
	ids := make([]string, len(found))
	for i, h := range found {
		ids[i] = h.id
	}
	return ids
}

func ids(messages []model.Message) []string {
	out := make([]string, len(messages))
	for i, msg := range messages {
		out[i] = msg.ID
	}
	return out
}

func TestEmitPendingStopsAtFiveMessagesInCreationOrder(t *testing.T) {
	f := newEmitFixture(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	// Reserved in one order, created in the reverse order: CreatedAt decides.
	all := make([]model.Message, 0, 7)
	for i := range 7 {
		all = append(all, f.queue(t, "body "+string(rune('a'+i)), base.Add(time.Duration(6-i)*time.Second)))
	}
	byCreated := make([]model.Message, len(all))
	for i, msg := range all {
		byCreated[len(all)-1-i] = msg
	}

	first, err := f.store.EmitPending(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := emitted(t, first, all), ids(byCreated[:emitMessageCap]); !slices.Equal(got, want) {
		t.Fatalf("first call emitted %v, want the five oldest %v", got, want)
	}
	for i, msg := range byCreated {
		current := f.read(t, msg.ID)
		wantStatus := emitHookPending
		if i < emitMessageCap {
			wantStatus = model.StatusContextEmitted
		}
		if current.Status != wantStatus || current.AcknowledgedAt != "" {
			t.Fatalf("message %d is %q (acknowledged %q), want %q unacknowledged", i, current.Status, current.AcknowledgedAt, wantStatus)
		}
	}
	second, err := f.store.EmitPending(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := emitted(t, second, all), ids(byCreated[emitMessageCap:]); !slices.Equal(got, want) {
		t.Fatalf("second call emitted %v, want the remaining two %v", got, want)
	}
	if third, err := f.store.EmitPending(f.target.ID); err != nil || third != "" {
		t.Fatalf("third call emitted %q (%v), want nothing", third, err)
	}
}

// contextBytes is what one message adds to the context.
func (f emitFixture) contextBytes(t *testing.T, id string) int {
	t.Helper()
	return len(model.Envelope(f.store.withLabel(f.read(t, id)))) + len("\n\n")
}

func TestEmitPendingStopsBeforeTheMessageThatWouldOverflow(t *testing.T) {
	f := newEmitFixture(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	big := strings.Repeat("x", 9*1024)
	first := f.queue(t, big, base)
	second := f.queue(t, big, base.Add(time.Second))
	third := f.queue(t, big, base.Add(2*time.Second))
	small := f.queue(t, "small", base.Add(3*time.Second))
	all := []model.Message{first, second, third, small}

	text, err := f.store.EmitPending(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := emitted(t, text, all), ids(all[:2]); !slices.Equal(got, want) {
		t.Fatalf("emitted %v, want the first two %v", got, want)
	}
	if len(text) > emitContextCap {
		t.Fatalf("context is %d bytes, over the %d cap", len(text), emitContextCap)
	}
	// The third did not fit, and the small one behind it is not skipped ahead.
	for _, msg := range []model.Message{third, small} {
		if current := f.read(t, msg.ID); current.Status != emitHookPending {
			t.Fatalf("%s is %q, want it still pending", msg.ID, current.Status)
		}
	}
	later, err := f.store.EmitPending(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := emitted(t, later, all), ids(all[2:]); !slices.Equal(got, want) {
		t.Fatalf("next call emitted %v, want the two that waited %v", got, want)
	}
}

func TestEmitPendingCapIsExactlyTwentyFourKiB(t *testing.T) {
	for _, extra := range []struct {
		name         string
		over         int
		wantSecondIn bool
	}{{"two messages that add up to exactly 24 KiB", 0, true}, {"one byte more", 1, false}} {
		t.Run(extra.name, func(t *testing.T) {
			f := newEmitFixture(t)
			base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			first := f.queue(t, strings.Repeat("x", 12000), base)
			second := f.queue(t, strings.Repeat("y", 12000), base.Add(time.Second))
			// Envelopes grow one byte per body byte, so resize the second body.
			delta := emitContextCap + extra.over - f.contextBytes(t, first.ID) - f.contextBytes(t, second.ID)
			f.rewrite(t, second.ID, func(m *model.Message) { m.Body = strings.Repeat("y", 12000+delta) })
			if total := f.contextBytes(t, first.ID) + f.contextBytes(t, second.ID); total != emitContextCap+extra.over {
				t.Fatalf("test setup: the two messages add up to %d bytes, want %d", total, emitContextCap+extra.over)
			}
			text, err := f.store.EmitPending(f.target.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := emitted(t, text, []model.Message{first, second})
			want := []string{first.ID}
			if extra.wantSecondIn {
				want = append(want, second.ID)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("emitted %v, want %v", got, want)
			}
		})
	}
}

func TestEmitPendingTakesOnlyThisSessionsUnacknowledgedHookMessages(t *testing.T) {
	f := newEmitFixture(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	wanted := f.queue(t, "wanted", base)
	acknowledged := f.queue(t, "already acknowledged", base.Add(time.Second))
	f.rewrite(t, acknowledged.ID, func(m *model.Message) { m.AcknowledgedAt = model.Timestamp() })
	submitted := f.queue(t, "already delivered", base.Add(2*time.Second))
	f.rewrite(t, submitted.ID, func(m *model.Message) { m.Status = model.StatusSubmitted })
	otherGeneration := f.queue(t, "for an older registration", base.Add(3*time.Second))
	f.rewrite(t, otherGeneration.ID, func(m *model.Message) { m.TargetGeneration = newID() })
	otherTarget := f.queue(t, "for another agent", base.Add(4*time.Second))
	f.rewrite(t, otherTarget.ID, func(m *model.Message) { m.To = f.sender.ID })

	text, err := f.store.EmitPending(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	all := []model.Message{wanted, acknowledged, submitted, otherGeneration, otherTarget}
	if got := emitted(t, text, all); !slices.Equal(got, []string{wanted.ID}) {
		t.Fatalf("emitted %v, want only %s", got, wanted.ID)
	}
	for msg, want := range map[string]model.DeliveryStatus{
		acknowledged.ID:    emitHookPending,
		submitted.ID:       model.StatusSubmitted,
		otherGeneration.ID: emitHookPending,
		otherTarget.ID:     emitHookPending,
		wanted.ID:          model.StatusContextEmitted,
	} {
		if current := f.read(t, msg); current.Status != want {
			t.Fatalf("%s is %q, want %q", msg, current.Status, want)
		}
	}
}

func TestEmitPendingRefusesAnAdapterWithoutHookDeliveryBeforeAnythingElse(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "codex")
	const want = "this adapter does not support hook delivery"
	if text, err := store.EmitPending(target.ID); err == nil || err.Error() != want || text != "" {
		t.Fatalf("EmitPending = %q, %v; want no context and %q", text, err, want)
	}
	// Two faults: the adapter check also comes before the unreachable-target
	// return, so a retired session of such an adapter still gets the error.
	if err := store.Update(target.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if text, err := store.EmitPending(target.ID); err == nil || err.Error() != want || text != "" {
		t.Fatalf("retired: EmitPending = %q, %v; want no context and %q", text, err, want)
	}
}

func TestEmitPendingForAnUnknownSessionIsAnError(t *testing.T) {
	store := fixtureStore(t)
	if text, err := store.EmitPending("cursor:" + newID()); !os.IsNotExist(err) || text != "" {
		t.Fatalf("EmitPending = %q, %v; want no context and a missing-session error", text, err)
	}
}

// Characterized, not endorsed: an unreachable target is not an error. The
// caller gets an empty context, the pending messages are left as they are,
// and no record is read.
func TestEmitPendingForAnUnreachableTargetReturnsNothingAndNoError(t *testing.T) {
	f := newEmitFixture(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	pending := f.queue(t, "waits forever", base)
	if err := f.store.Update(f.target.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	// An unreadable record is not even read for an unreachable target.
	garbage := filepath.Join(f.store.Dir, "messages", newID()+".json")
	if err := os.WriteFile(garbage, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := f.store.EmitPending(f.target.ID)
	if err != nil || text != "" {
		t.Fatalf("EmitPending = %q, %v; want an empty context and no error", text, err)
	}
	if current := f.read(t, pending.ID); current.Status != emitHookPending || current.AcknowledgedAt != "" {
		t.Fatalf("pending message changed: %q", current.Status)
	}
}

// Characterized, not endorsed: on its way out, the unreachable-target return
// settles messages that were queued for approval.
func TestEmitPendingForAnUnreachableTargetSettlesQueuedApprovals(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	sender, key, target, _ := registerPair(t, store, "codex", "cursor")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	held, err := service.Send(context.Background(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "waits for the prompt"})
	if err != nil || held.Status != model.StatusApprovalBlocked || !held.Held {
		t.Fatalf("setup: %+v %v", held, err)
	}
	if err := store.Update(target.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	// Retiring does not settle by itself; EmitPending does.
	if !storedMessage(t, store, held.ID).Held {
		t.Fatal("retirement already settled the message")
	}
	if text, err := store.EmitPending(target.ID); err != nil || text != "" {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	after := storedMessage(t, store, held.ID)
	if after.Held || after.Status != model.StatusNotSent || after.Receipt == nil || after.Receipt.Error != "destination has retired" {
		t.Fatalf("queued approval after EmitPending = %+v", after)
	}
}

func storedMessage(t *testing.T, store Store, id string) model.Message {
	t.Helper()
	msg, err := store.message(id)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestEmitPendingForALiveTargetSkipsAnUnreadableRecord(t *testing.T) {
	f := newEmitFixture(t)
	pending := f.queue(t, "still delivered", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	garbage := filepath.Join(f.store.Dir, "messages", newID()+".json")
	if err := os.WriteFile(garbage, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := f.store.EmitPending(f.target.ID)
	if err != nil || !strings.Contains(text, pending.ID) {
		t.Fatalf("EmitPending = %q, %v; want the readable message and no error", text, err)
	}
	if current := f.read(t, pending.ID); current.Status != model.StatusContextEmitted {
		t.Fatalf("readable message status %q", current.Status)
	}
}
