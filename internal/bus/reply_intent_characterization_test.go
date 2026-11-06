package bus_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

// replyFixture is a state directory with one incoming message that has not
// been answered yet, plus the reply record that a real Reply would reserve.
type replyFixture struct {
	store  bus.Store
	parent bus.Message
	// reply is the persisted JSON of the reservation for the reply "pong".
	reply map[string]any
	// replier is the recipient of the parent message.
	replier   agent
	transport *fakeAdapter
}

// newReplyFixture learns the reply record from a real Reply on a copy of the
// state, so the tests need no knowledge of how reply IDs are derived.
func newReplyFixture(t *testing.T) replyFixture {
	t.Helper()
	target := nativeAdapter("alpha")
	store := newStore(t, target, nativeAdapter("beta"))
	asker, replier := register(t, store, "alpha"), register(t, store, "beta")
	svc := bus.Service{Store: store}
	parent, err := svc.Send(context.Background(), bus.SendRequest{Actor: asker.ID, Capability: asker.Key, To: replier.ID, Body: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	scratch := restart(t, store)
	scratch.Dir = filepath.Join(t.TempDir(), "state")
	copyDir(t, store.Dir, scratch.Dir)
	reply, err := (&bus.Service{Store: scratch}).Reply(context.Background(), bus.ReplyRequest{Actor: replier.ID, Capability: replier.Key, MessageID: parent.ID, Body: "pong"})
	if err != nil {
		t.Fatal(err)
	}
	// A durable intent holds the pre-delivery reservation, not the final
	// submitted reply. Recreate it through the public reservation API using
	// the opaque ID returned above; no private ID derivation is duplicated.
	reservation := restart(t, store)
	reservation.Dir = filepath.Join(t.TempDir(), "state")
	copyDir(t, store.Dir, reservation.Dir)
	reserved, _, fresh, err := reservation.ReserveMessage(bus.SendRequest{
		Actor: replier.ID, Capability: replier.Key, To: asker.ID,
		MessageID: reply.ID, Body: "pong", ReplyTo: parent.ID,
	})
	if err != nil || !fresh || reserved.Status != "attempting" || reserved.Receipt != nil {
		t.Fatalf("reply reservation: status %q receipt %+v fresh %v err %v", reserved.Status, reserved.Receipt, fresh, err)
	}
	return replyFixture{
		store:     store,
		parent:    parent,
		reply:     readJSONFile(t, filepath.Join(reservation.Dir, "messages", reply.ID+".json")),
		replier:   replier,
		transport: target,
	}
}

// intent builds the durable reply intent a Reply leaves before it commits.
func (f replyFixture) intent() map[string]any {
	reply := map[string]any{}
	for key, value := range f.reply {
		reply[key] = value
	}
	return map[string]any{
		"schema":          1,
		"parent_id":       f.parent.ID,
		"acknowledged_at": "2026-11-06T09:00:00.000000000Z",
		"reply":           reply,
	}
}

func (f replyFixture) intentPath() string {
	return filepath.Join(f.store.Dir, "transactions", f.reply["id"].(string)+".json")
}

func (f replyFixture) writeIntent(t *testing.T, tx map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.intentPath()), 0700); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, f.intentPath(), tx)
}

// TestDamagedReplyIntentBlocksReadsAndSendsWithoutChangingState pins how
// recovery treats an intent file that does not describe a valid reply: the
// caller sees a stable error code, no record changes, and the damaged file
// stays so the next locked operation fails the same way.
func TestDamagedReplyIntentBlocksReadsAndSendsWithoutChangingState(t *testing.T) {
	tests := []struct {
		name   string
		damage func(tx map[string]any)
		want   bus.ErrorCode
	}{
		{"unsupported intent schema", func(tx map[string]any) { tx["schema"] = 2 }, "unsupported_schema"},
		{"unsupported reply record schema", func(tx map[string]any) { tx["reply"].(map[string]any)["schema"] = 99 }, "unsupported_schema"},
		{"parent differs from reply_to", func(tx map[string]any) { tx["parent_id"] = "11111111-1111-4111-8111-111111111111" }, "invalid_identity"},
		{"missing acknowledgement time", func(tx map[string]any) { tx["acknowledged_at"] = "" }, "invalid_identity"},
		{"body does not match the retry identity", func(tx map[string]any) { tx["reply"].(map[string]any)["body"] = "changed" }, "invalid_identity"},
		{"sender does not match the retry identity", func(tx map[string]any) {
			tx["reply"].(map[string]any)["from"] = "beta:11111111-1111-4111-8111-111111111111"
		}, "invalid_identity"},
		{"reply id is not a UUID", func(tx map[string]any) { tx["reply"].(map[string]any)["id"] = "not-a-uuid" }, "invalid_identity"},
		{"unknown delivery status", func(tx map[string]any) { tx["reply"].(map[string]any)["status"] = "delivered_maybe" }, "invalid_transition"},
		{"reply addressed to someone else", func(tx map[string]any) {
			tx["reply"].(map[string]any)["to"] = "alpha:11111111-1111-4111-8111-111111111111"
		}, "conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newReplyFixture(t)
			tx := f.intent()
			test.damage(tx)
			// Keep the file name equal to the original reply ID, so the
			// damage under test is the content and not the path.
			f.writeIntent(t, tx)
			before := snapshot(t, f.store.Dir)

			restarted := restart(t, f.store)
			for attempt := 1; attempt <= 2; attempt++ {
				if _, err := restarted.List(); bus.ErrorCodeOf(err) != test.want {
					t.Fatalf("attempt %d: List error code = %q (%v), want %q", attempt, bus.ErrorCodeOf(err), err, test.want)
				}
			}
			svc := bus.Service{Store: restarted}
			if _, err := svc.Send(context.Background(), bus.SendRequest{Actor: f.replier.ID, Capability: f.replier.Key, To: f.parent.From, Body: "later"}); bus.ErrorCodeOf(err) != test.want {
				t.Fatalf("Send error code = %q (%v), want %q: a damaged intent must not be skipped", bus.ErrorCodeOf(err), err, test.want)
			}
			if after := snapshot(t, f.store.Dir); !maps.Equal(before, after) {
				t.Fatalf("state changed while rejecting a damaged intent:\nbefore %v\nafter  %v", keys(before), keys(after))
			}

			// Removing the damaged file restores normal operation.
			if err := os.Remove(f.intentPath()); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.List(); err != nil {
				t.Fatalf("List after removing the damaged intent: %v", err)
			}
		})
	}
}

func TestReplyIntentForAMissingParentIsNotFound(t *testing.T) {
	f := newReplyFixture(t)
	f.writeIntent(t, f.intent())
	if err := os.Remove(filepath.Join(f.store.Dir, "messages", f.parent.ID+".json")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, f.store.Dir)
	if _, err := restart(t, f.store).List(); bus.ErrorCodeOf(err) != "not_found" {
		t.Fatalf("List error code = %q (%v), want %q", bus.ErrorCodeOf(err), err, "not_found")
	}
	if after := snapshot(t, f.store.Dir); !maps.Equal(before, after) {
		t.Fatal("state changed while rejecting an intent without a parent")
	}
}

func TestReplyIntentConflictingWithACommittedReservationIsRejected(t *testing.T) {
	f := newReplyFixture(t)
	// A reservation with the same ID but another target generation exists.
	existing := map[string]any{}
	for key, value := range f.reply {
		existing[key] = value
	}
	existing["target_generation"] = "11111111-1111-4111-8111-111111111111"
	writeJSONFile(t, filepath.Join(f.store.Dir, "messages", f.reply["id"].(string)+".json"), existing)
	f.writeIntent(t, f.intent())
	before := snapshot(t, f.store.Dir)
	if _, err := restart(t, f.store).List(); bus.ErrorCodeOf(err) != "conflict" {
		t.Fatalf("List error code = %q (%v), want %q", bus.ErrorCodeOf(err), err, "conflict")
	}
	if after := snapshot(t, f.store.Dir); !maps.Equal(before, after) {
		t.Fatal("state changed while rejecting a conflicting reservation")
	}
}

// TestReplyIntentIsAppliedOnceAndKeepsItsAcknowledgementTime pins the replay of
// a valid intent: the first locked operation completes it, the intent's
// acknowledgement time reaches the parent, and later recoveries change nothing.
func TestReplyIntentIsAppliedOnceAndKeepsItsAcknowledgementTime(t *testing.T) {
	f := newReplyFixture(t)
	tx := f.intent()
	f.writeIntent(t, tx)

	if _, err := restart(t, f.store).List(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.intentPath()); !os.IsNotExist(err) {
		t.Fatal("recovered intent was not removed", err)
	}
	parent := readJSONFile(t, filepath.Join(f.store.Dir, "messages", f.parent.ID+".json"))
	if parent["acknowledged_at"] != tx["acknowledged_at"] {
		t.Fatalf("parent acknowledged_at = %v, want the intent's %v", parent["acknowledged_at"], tx["acknowledged_at"])
	}
	reply := readJSONFile(t, filepath.Join(f.store.Dir, "messages", f.reply["id"].(string)+".json"))
	if reply["body"] != "pong" || reply["reply_to"] != f.parent.ID || reply["status"] != "attempting" || reply["receipt"] != nil {
		t.Fatalf("recovered reply = %v", reply)
	}
	// Recovery has no proof whether the interrupted process submitted the
	// reservation. Retrying this reply must not invoke the transport again.
	calls := f.transport.transportCalls()
	if _, err := (&bus.Service{Store: restart(t, f.store)}).Reply(context.Background(), bus.ReplyRequest{
		Actor: f.replier.ID, Capability: f.replier.Key, MessageID: f.parent.ID, Body: "pong",
	}); err != nil || f.transport.transportCalls() != calls {
		t.Fatalf("recovered reply retry: err %v, transport calls %d -> %d", err, calls, f.transport.transportCalls())
	}
	settled := snapshot(t, f.store.Dir)
	for range 2 {
		if _, err := restart(t, f.store).List(); err != nil {
			t.Fatal(err)
		}
	}
	if after := snapshot(t, f.store.Dir); !maps.Equal(settled, after) {
		t.Fatal("repeated recovery changed the settled state")
	}
	var count int
	for name := range settled {
		if filepath.Dir(name) == "messages" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("journal holds %d messages, want the parent and exactly one reply", count)
	}
}

func keys(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
