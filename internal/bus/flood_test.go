package bus

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func spendFlood(store Store, msg Message, now time.Time) error {
	return store.locked(func() error { return store.consumeFloodQuota(msg, now) })
}

func floodMessage() Message {
	return Message{ID: newID(), From: "codex:" + newID(), To: "cursor:" + newID()}
}

func floodBytes(t *testing.T, store Store, target string) []byte {
	t.Helper()
	path, err := store.floodPath(target)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFloodBurstRefillIsolationAndClockRollback(t *testing.T) {
	store := fixtureStore(t)
	msg := floodMessage()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for range 12 {
		msg.ID = newID()
		if err := spendFlood(store, msg, now); err != nil {
			t.Fatal(err)
		}
	}
	before := floodBytes(t, store, msg.To)
	for _, at := range []time.Time{now, now.Add(-time.Hour), now.Add(5*time.Second - time.Nanosecond)} {
		msg.ID = newID()
		err := spendFlood(store, msg, at)
		if ErrorCodeOf(err) != CodeRateLimited || !strings.Contains(err.Error(), "pair short bucket") || !strings.Contains(err.Error(), "retry in") {
			t.Fatal("pair did not refuse with a retry time", err)
		}
		if string(before) != string(floodBytes(t, store, msg.To)) {
			t.Fatal("refusal changed quota state")
		}
	}
	msg.ID = newID()
	if err := spendFlood(store, msg, now.Add(5*time.Second)); err != nil {
		t.Fatal("one token did not refill at the boundary", err)
	}
	otherSender := msg
	otherSender.From, otherSender.ID = "claude:"+newID(), newID()
	if err := spendFlood(store, otherSender, now.Add(5*time.Second)); err != nil {
		t.Fatal("separate sender shared pair quota", err)
	}
	otherTarget := msg
	otherTarget.To, otherTarget.ID = "cursor:"+newID(), newID()
	if err := spendFlood(store, otherTarget, now); err != nil {
		t.Fatal("separate target shared quota", err)
	}
}

func TestFloodInboundCapDoesNotSpendRejectedSenderQuota(t *testing.T) {
	store := fixtureStore(t)
	msg := floodMessage()
	now := time.Now().UTC()
	for range 48 {
		msg.From, msg.ID = "codex:"+newID(), newID()
		if err := spendFlood(store, msg, now); err != nil {
			t.Fatal(err)
		}
	}
	msg.From, msg.ID = "codex:"+newID(), newID()
	before := floodBytes(t, store, msg.To)
	if err := spendFlood(store, msg, now); ErrorCodeOf(err) != CodeRateLimited || !strings.Contains(err.Error(), "inbound short bucket") {
		t.Fatal("inbound cap did not combine senders", err)
	}
	if string(before) != string(floodBytes(t, store, msg.To)) {
		t.Fatal("inbound refusal consumed sender quota")
	}
	if err := spendFlood(store, msg, now.Add(1500*time.Millisecond)); err != nil {
		t.Fatal("inbound token did not refill", err)
	}
}

func TestFloodHourlyCapsAndExpiry(t *testing.T) {
	for _, scope := range []string{"pair", "inbound"} {
		t.Run(scope, func(t *testing.T) {
			store := fixtureStore(t)
			msg := floodMessage()
			now := time.Now().UTC()
			limit, interval := 120, 6*time.Second
			senders := []string{msg.From}
			if scope == "inbound" {
				limit, interval = 400, 2*time.Second
				for range 4 {
					senders = append(senders, "codex:"+newID())
				}
			}
			for i := range limit {
				msg.From, msg.ID = senders[i%len(senders)], newID()
				if err := spendFlood(store, msg, now.Add(time.Duration(i)*interval)); err != nil {
					t.Fatalf("admission %d: %v", i, err)
				}
			}
			msg.ID = newID()
			if err := spendFlood(store, msg, now.Add(time.Duration(limit)*interval)); ErrorCodeOf(err) != CodeRateLimited || !strings.Contains(err.Error(), scope+" hourly cap") {
				t.Fatal("slow traffic bypassed hourly cap", err)
			}
			if len(floodBytes(t, store, msg.To)) > 128*1024 {
				t.Fatal("bounded quota file exceeded state reader limit")
			}
			if err := spendFlood(store, msg, now.Add(time.Hour)); err != nil {
				t.Fatal("oldest hourly slot did not expire at the boundary", err)
			}
		})
	}
}

func TestFloodCrossStoreConcurrency(t *testing.T) {
	store := fixtureStore(t)
	msg := floodMessage()
	now := time.Now().UTC()
	var admitted atomic.Int32
	errors := make(chan error, 40)
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := Store{Dir: store.Dir, Registry: testRegistry()}
			attempt := msg
			attempt.ID = newID()
			if err := spendFlood(other, attempt, now); err != nil {
				errors <- err
			} else {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if ErrorCodeOf(err) != CodeRateLimited {
			t.Fatal(err)
		}
	}
	if admitted.Load() != 12 {
		t.Fatalf("concurrent stores admitted %d, want %d", admitted.Load(), 12)
	}
}

func TestFloodRetryReservationsAndIdleSenderExpiry(t *testing.T) {
	store := fixtureStore(t)
	msg := floodMessage()
	now := time.Now().UTC()
	if err := spendFlood(store, msg, now); err != nil {
		t.Fatal(err)
	}
	before := floodBytes(t, store, msg.To)
	if err := spendFlood(store, msg, now.Add(time.Second)); err != nil || string(before) != string(floodBytes(t, store, msg.To)) {
		t.Fatal("retry before message/intent commit spent quota twice", err)
	}
	msg.From = "codex:" + newID()
	if err := spendFlood(store, msg, now); ErrorCodeOf(err) != CodeConflict {
		t.Fatal("another sender reused a reserved ID", err)
	}
	msg.ID = newID()
	if err := spendFlood(store, msg, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	path, _ := store.floodPath(msg.To)
	var state floodRecord
	if err := readJSON(path, &state); err != nil || len(state.Senders) != 1 || len(state.Recent) != 2 {
		t.Fatal("idle senders accumulated or hourly history was dropped", err)
	}
}

func TestFloodDenialDoesNotReserveOrAcknowledgeReplies(t *testing.T) {
	for _, retired := range []bool{false, true} {
		service := newTestService(t)
		store := service.Store
		actor, key, target, _ := registerPair(t, store, "cursor", "codex")
		parent := Message{ID: newID(), From: target.ID, To: actor.ID, Body: "review", Status: StatusSubmitted}
		if err := store.saveMessage(parent); err != nil {
			t.Fatal(err)
		}
		if retired {
			if err := store.Update(target.ID, StateRetired, true); err != nil {
				t.Fatal(err)
			}
		}
		// Freeze exhausted quota ahead of the wall clock, avoiding sleeps and
		// flaky boundary assertions when the race suite runs under load.
		for range 12 {
			if err := spendFlood(store, Message{ID: newID(), From: actor.ID, To: target.ID}, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}

		refused, err := service.Reply(context.Background(), ReplyRequest{Actor: actor.ID, Capability: key, MessageID: parent.ID, Body: "reply"})
		if ErrorCodeOf(err) != CodeRateLimited || refused.Receipt == nil || refused.Receipt.Code != CodeRateLimited {
			t.Fatal("reply bypassed quota", err)
		}
		stored, err := store.message(parent.ID)
		if err != nil || stored.AcknowledgedAt != "" {
			t.Fatal("rate refusal acknowledged the incoming message", err)
		}
		if _, err := store.message(replyID(actor.ID, parent.ID, "reply")); !os.IsNotExist(err) {
			t.Fatal("rate refusal added an outgoing journal entry", err)
		}
	}
}

func TestFloodKnownMessageRetrySkipsQuotaAndNewSendAvoidsJournalScan(t *testing.T) {
	store := fixtureStore(t)
	actor, key, target, _ := registerPair(t, store, "cursor", "codex")
	request := SendRequest{Actor: actor.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "work"}
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", newID()+".json"), []byte("unrelated malformed record"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, fresh, err := store.ReserveMessage(request); err != nil || !fresh {
		t.Fatal("new send scanned the unrelated journal", err)
	}
	path, _ := store.floodPath(target.ID)
	if err := os.WriteFile(path, []byte("invalid quota"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, fresh, err := store.ReserveMessage(request); err != nil || fresh {
		t.Fatal("known-ID retry consulted or consumed quota", err)
	}
	request.MessageID = newID()
	if _, _, _, err := store.ReserveMessage(request); err == nil {
		t.Fatal("new send ignored corrupt quota state")
	}
}

func TestFloodWatchNoticesAreExempt(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	watcher, key, subject, _ := registerPair(t, store, "cursor", "codex")
	for range 12 {
		if err := spendFlood(store, Message{ID: newID(), From: subject.ID, To: watcher.ID}, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	before := floodBytes(t, store, watcher.ID)

	if _, err := service.Watch(watcher.ID, key, subject.ID); err != nil {
		t.Fatal(err)
	}
	stopHook(t, service, "codex", subject.ID)
	items, err := store.Messages(watcher.ID, key)
	if err != nil || len(items) != 1 || items[0].Kind != noticeKind || string(before) != string(floodBytes(t, store, watcher.ID)) {
		t.Fatal("exhausted agent quota blocked or charged a watch notice", err)
	}
}

func TestFloodSendRefusalNeverCreatesAnApprovalHold(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	actor, key, target, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(target.ID, StateApproval, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	for range 12 {
		if err := spendFlood(store, Message{ID: newID(), From: actor.ID, To: target.ID}, now); err != nil {
			t.Fatal(err)
		}
	}
	before := floodBytes(t, store, target.ID)

	msg, err := service.Send(context.Background(), SendRequest{Actor: actor.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "limited"})
	if ErrorCodeOf(err) != CodeRateLimited || msg.Held || msg.Status != StatusNotSent || msg.Receipt == nil || msg.Receipt.Code != CodeRateLimited {
		t.Fatalf("rate refusal looked like a queued message: %+v %v", msg, err)
	}
	if _, err := store.message(msg.ID); !os.IsNotExist(err) {
		t.Fatal("rate refusal grew the journal", err)
	}
	current, err := store.session(target.ID)
	if err != nil || current.ApprovalHeld != 0 || string(before) != string(floodBytes(t, store, target.ID)) {
		t.Fatal("rate refusal queued or consumed quota", err)
	}
}

func TestFloodPartialOrUnsupportedStateFailsClosed(t *testing.T) {
	store := fixtureStore(t)
	msg := floodMessage()
	now := time.Now().UTC()
	if err := spendFlood(store, msg, now); err != nil {
		t.Fatal(err)
	}
	path, _ := store.floodPath(msg.To)
	for _, invalid := range []string{`{}`, `{"schema":99}`, `{"schema":1}`, `{"schema":1,"senders":{},"target":"other"}`} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		msg.ID = newID()
		if err := spendFlood(store, msg, now); err == nil {
			t.Fatalf("partial quota silently reset to full: %s", invalid)
		}
		if string(floodBytes(t, store, msg.To)) != invalid {
			t.Fatal("invalid quota state was overwritten")
		}
	}
}
