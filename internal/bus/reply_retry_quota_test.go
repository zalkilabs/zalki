package bus

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// These tests pin how a reply to an unavailable destination uses the flood
// quota (prepareReply, message_reply.go). The reply is recorded as not sent and
// still acknowledges the parent (characterized, not endorsed). Its record
// takes quota only the first time: a retry with the same body finds the record
// and must not take quota again, even when the quota is used up, while a reply
// with a new body is a new record and does take quota.

func floodRecordOf(t *testing.T, store Store, target string) floodRecord {
	t.Helper()
	var state floodRecord
	if err := json.Unmarshal(floodBytes(t, store, target), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// exhaustSender leaves the sender's short-term bucket empty for the target.
func exhaustSender(t *testing.T, store Store, target, sender string) {
	t.Helper()
	state := floodRecordOf(t, store, target)
	state.Senders[derivedID("zalki/flood/sender/v1", sender)] = model.FloodBucket{Tokens: 0, At: time.Now().UTC().Add(time.Hour)}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.floodPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// ageArrivals moves every recorded arrival back by the given time, as if the
// replies had been recorded that long ago.
func ageArrivals(t *testing.T, store Store, target string, by time.Duration) {
	t.Helper()
	state := floodRecordOf(t, store, target)
	for i := range state.Recent {
		state.Recent[i].At = state.Recent[i].At.Add(-by)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.floodPath(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// refusedReplyFixture is a parent message whose sender cannot receive the
// reply any more, because its session is retired or gone.
type refusedReplyFixture struct {
	t       *testing.T
	service Service
	from    Session
	to      Session
	key     string
	parent  Message
}

func newRefusedReplyFixture(t *testing.T, missing bool) refusedReplyFixture {
	t.Helper()
	service := newTestService(t)
	store := service.Store
	from, _, to, key := registerPair(t, store, "codex", "cursor")
	parent := Message{ID: newID(), From: from.ID, To: to.ID, Body: "ping", Status: StatusSubmitted}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	if missing {
		path, _ := store.sessionPath(from.ID)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	} else if err := store.Update(from.ID, StateRetired, true); err != nil {
		t.Fatal(err)
	}
	return refusedReplyFixture{t: t, service: service, from: from, to: to, key: key, parent: parent}
}

func (f refusedReplyFixture) reply(body string) (Message, error) {
	return f.service.Reply(context.Background(), ReplyRequest{Actor: f.to.ID, Capability: f.key, MessageID: f.parent.ID, Body: body})
}

func (f refusedReplyFixture) flood() []byte { return floodBytes(f.t, f.service.Store, f.from.ID) }

// requireRetryTakesNothing sends the same reply again and checks that it finds
// the recorded refusal and leaves the flood state as it was.
func (f refusedReplyFixture) requireRetryTakesNothing(first Message, when, change string) {
	f.t.Helper()
	before := f.flood()
	again, err := f.reply("pong")
	if err != nil || again.ID != first.ID {
		f.t.Fatalf("retry %s = %+v, %v; want the recorded refusal", when, again, err)
	}
	if string(f.flood()) != string(before) {
		f.t.Fatal(change)
	}
}

// requireOneArrivalForTheFirstReply checks the quota after the first refusal.
func (f refusedReplyFixture) requireOneArrivalForTheFirstReply(first Message) {
	f.t.Helper()
	sender := derivedID("zalki/flood/sender/v1", f.to.ID)
	state := floodRecordOf(f.t, f.service.Store, f.from.ID)
	if len(state.Recent) != 1 || state.Recent[0].ID != first.ID || state.Recent[0].Sender != sender {
		f.t.Fatalf("the first reply left arrivals %+v, want one for %s", state.Recent, first.ID)
	}
}

func TestRefusedReplyRetryDoesNotSpendFloodQuotaAgain(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "destination retired"
		if missing {
			name = "destination session missing"
		}
		t.Run(name, func(t *testing.T) {
			f := newRefusedReplyFixture(t, missing)
			store := f.service.Store

			first, err := f.reply("pong")
			if err != nil || first.Status != StatusNotSent {
				t.Fatalf("first reply = %+v, %v; want a recorded refusal", first, err)
			}
			f.requireOneArrivalForTheFirstReply(first)
			recordAfterFirst := mustRead(t, store.Dir, "messages", first.ID+".json")

			// A retry with the same body finds its record and takes no quota.
			f.requireRetryTakesNothing(first, "", "a retry of a recorded refusal took flood quota again")
			if string(mustRead(t, store.Dir, "messages", first.ID+".json")) != string(recordAfterFirst) {
				t.Fatal("a retry rewrote the recorded refusal")
			}

			// After the arrival has left the one-hour window the quota no longer
			// remembers the reply, but the retry must still find the record first
			// and take nothing.
			ageArrivals(t, store, f.from.ID, 2*time.Hour)
			f.requireRetryTakesNothing(first, "after the window", "a retry of an old recorded refusal took flood quota again")

			// Even with the sender's quota used up, the retry still succeeds ...
			exhaustSender(t, store, f.from.ID, f.to.ID)
			f.requireRetryTakesNothing(first, "with the quota used up", "a retry changed the flood state")
			// ... while a reply with a new body is a new record that needs quota.
			used := f.flood()
			other, err := f.reply("a different reply")
			if ErrorCodeOf(err) != CodeRateLimited {
				t.Fatalf("new reply with the quota used up = %+v, %v; want a rate limit", other, err)
			}
			if string(f.flood()) != string(used) {
				t.Fatal("a rate-limited reply changed the flood state")
			}
		})
	}
}

func TestRefusedReplyWithANewBodySpendsOneMoreArrival(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	from, _, to, key := registerPair(t, store, "codex", "cursor")
	parent := Message{ID: newID(), From: from.ID, To: to.ID, Body: "ping", Status: StatusSubmitted}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(from.ID, StateRetired, true); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 3)
	for _, body := range []string{"one", "two", "three"} {
		reply, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: key, MessageID: parent.ID, Body: body})
		if err != nil || reply.Status != StatusNotSent {
			t.Fatalf("reply %q = %+v, %v", body, reply, err)
		}
		ids = append(ids, reply.ID)
		if got := len(floodRecordOf(t, store, from.ID).Recent); got != len(ids) {
			t.Fatalf("after %d new replies the quota has %d arrivals", len(ids), got)
		}
	}
	for i, arrival := range floodRecordOf(t, store, from.ID).Recent {
		if arrival.ID != ids[i] {
			t.Fatalf("arrival %d is %s, want %s", i, arrival.ID, ids[i])
		}
	}
}

func mustRead(t *testing.T, parts ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
