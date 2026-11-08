package bus_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// These tests pin which error wins when a reply has more than one fault.
// Single faults are pinned by the tests in reply_intent_characterization_test.go
// (codes only); this file adds the exact texts and the two-faults-at-once
// cases. Exact error text and order are characterized, not endorsed.
//
// Recovery of a stored reply intent (applyReplyTransaction) checks, in order:
//  1. intent schema, or reply record schema out of range: unsupported schema
//  2. reply ID is a UUID
//  3. intent file name, parent ID and acknowledgement time: invalid identity
//  4. reply ID equals the ID derived from sender, parent and body: retry identity
//  5. reply status is valid: invalid transition
//  6. the parent message exists: not found
//  7. the parent matches (recipient, sender, can still be acknowledged): conflict
//  8. a committed reply record with this ID agrees: conflict
// Only then are the reservation, the acknowledgement and the intent file written.
//
// A reply request (prepareReply) checks, in order, under the journal lock:
//  1. the actor: not retired, owner not exited, capability right
//  2. the parent message exists
//  3. the actor is the parent's recipient
//  4. the parent is not an automatic notice
//  5. the parent can still be acknowledged (it was sent)
// Only then does it reserve the reply, apply flood limits and write the intent.
// Every refusal above leaves the journal exactly as it was.

const (
	errSchema      = "unsupported_schema: unsupported reply transaction schema"
	errNotUUID     = "invalid_identity: expected an exact session/message UUID"
	errIdentity    = "invalid_identity: invalid reply transaction identity"
	errRetry       = "invalid_identity: reply transaction has an invalid retry identity"
	errStatus      = "invalid_transition: invalid reply transaction status"
	errNotMatching = "conflict: reply transaction does not match its parent"
	errReservation = "conflict: reply reservation conflicts with committed transaction"
)

const otherReplyID = "11111111-1111-4111-8111-111111111111"

func replyRecord(tx map[string]any) map[string]any { return tx["reply"].(map[string]any) }

// existingReservation writes a committed reply record that disagrees with the
// intent (another target generation).
func existingReservation(t *testing.T, f replyFixture) {
	t.Helper()
	existing := maps.Clone(f.reply)
	existing["target_generation"] = otherReplyID
	writeJSONFile(t, filepath.Join(f.store.Dir, "messages", f.reply["id"].(string)+".json"), existing)
}

func removeParent(t *testing.T, f replyFixture) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.store.Dir, "messages", f.parent.ID+".json")); err != nil {
		t.Fatal(err)
	}
}

func removeParentAndReserve(t *testing.T, f replyFixture) {
	t.Helper()
	removeParent(t, f)
	existingReservation(t, f)
}

// intentCase damages a reply intent, optionally changes the journal as well,
// and names the error that must win: an exact text, or a code when text is empty.
type intentCase struct {
	name   string
	damage func(tx map[string]any)
	extra  func(t *testing.T, f replyFixture)
	want   string
	code   model.ErrorCode
}

func intentCases() []intentCase {
	to := func(tx map[string]any) { replyRecord(tx)["to"] = "alpha:" + otherReplyID }
	status := func(tx map[string]any) { replyRecord(tx)["status"] = "delivered_maybe" }
	body := func(tx map[string]any) { replyRecord(tx)["body"] = "changed" }
	noAck := func(tx map[string]any) { tx["acknowledged_at"] = "" }
	notUUID := func(tx map[string]any) { replyRecord(tx)["id"] = "not-a-uuid" }
	badSchema := func(tx map[string]any) { tx["schema"] = 2 }
	both := func(first, second func(map[string]any)) func(map[string]any) {
		return func(tx map[string]any) { first(tx); second(tx) }
	}
	return []intentCase{
		// One fault each, for the exact texts.
		{name: "reservation of another generation", extra: existingReservation, want: errReservation},
		{name: "reply addressed to someone else", damage: to, want: errNotMatching},
		{name: "unsupported schema", damage: badSchema, want: errSchema},
		// Two faults at once: the earlier check wins.
		{name: "bad intent schema and reply id not a UUID", damage: both(badSchema, notUUID), want: errSchema},
		{name: "bad reply schema and unknown status", damage: both(func(tx map[string]any) { replyRecord(tx)["schema"] = 99 }, status), want: errSchema},
		{name: "reply id not a UUID and missing acknowledgement time", damage: both(notUUID, noAck), want: errNotUUID},
		{name: "missing acknowledgement time and changed body", damage: both(noAck, body), want: errIdentity},
		{name: "parent differs from reply_to and changed body", damage: both(func(tx map[string]any) { tx["parent_id"] = otherReplyID }, body), want: errIdentity},
		{name: "changed body and unknown status", damage: both(body, status), want: errRetry},
		{name: "unknown status and missing parent", damage: status, extra: removeParent, want: errStatus},
		{name: "missing parent and conflicting reservation", extra: removeParentAndReserve, code: "not_found"},
		{name: "wrong recipient and conflicting reservation", damage: to, extra: existingReservation, want: errNotMatching},
	}
}

// assertRecoveryError checks the error of one recovery attempt.
func assertRecoveryError(t *testing.T, attempt int, err error, test intentCase) {
	t.Helper()
	switch {
	case test.code != "" && model.ErrorCodeOf(err) != test.code:
		t.Fatalf("attempt %d: error code = %q (%v), want %q", attempt, model.ErrorCodeOf(err), err, test.code)
	case test.code == "" && (err == nil || err.Error() != test.want):
		t.Fatalf("attempt %d: error = %v, want %q", attempt, err, test.want)
	}
}

func TestReplyIntentWithSeveralFaultsReportsTheFirstCheck(t *testing.T) {
	for _, test := range intentCases() {
		t.Run(test.name, func(t *testing.T) {
			f := newReplyFixture(t)
			tx := f.intent()
			if test.damage != nil {
				test.damage(tx)
			}
			// The file keeps the original reply ID as its name, so the damage
			// under test is the content (the intent path is checked in step 3).
			f.writeIntent(t, tx)
			if test.extra != nil {
				test.extra(t, f)
			}
			before := snapshot(t, f.store.Dir)
			restarted := restart(t, f.store)
			for attempt := 1; attempt <= 2; attempt++ {
				_, err := restarted.List()
				assertRecoveryError(t, attempt, err, test)
			}
			if after := snapshot(t, f.store.Dir); !maps.Equal(before, after) {
				t.Fatalf("state changed while rejecting a damaged intent:\nbefore %v\nafter  %v", keys(before), keys(after))
			}
		})
	}
}

// parentOf rewrites the stored parent message of the fixture.
func parentOf(t *testing.T, f replyFixture, change func(parent map[string]any)) {
	t.Helper()
	path := filepath.Join(f.store.Dir, "messages", f.parent.ID+".json")
	parent := readJSONFile(t, path)
	change(parent)
	writeJSONFile(t, path, parent)
}

const (
	errWrongKey = "unauthorized: wrong session capability; use your own SessionStart context"
	errRetired  = "unavailable: session is retired; registration is required"
	errNotBound = "unauthorized: only the bound recipient can reply"
	errNotSent  = "invalid_transition: cannot acknowledge a message that was not sent"
)

func asNotice(parent map[string]any) { parent["kind"] = "notice" }
func notSent(parent map[string]any)  { parent["status"] = "not_sent" }
func noticeNotSent(parent map[string]any) {
	asNotice(parent)
	notSent(parent)
}

func retireReplier(t *testing.T, f replyFixture) {
	t.Helper()
	if err := f.store.Update(f.replier.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
}

func retireAsker(t *testing.T, f replyFixture) {
	t.Helper()
	if err := f.store.Update(f.parent.From, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
}

// requestCase prepares a parent message, then replies as the recipient or as
// another agent, and names the error that must win (an exact text, in which
// PARENT_FROM stands for the parent's sender, or a code when text is empty).
type requestCase struct {
	name        string
	asRecipient bool
	wrongKey    bool
	retire      func(t *testing.T, f replyFixture)
	parent      func(parent map[string]any)
	parentID    string
	want        string
	code        model.ErrorCode
}

func requestCases() []requestCase {
	const missingParent = "11111111-1111-4111-8111-111111111111"
	return []requestCase{
		{name: "wrong capability and unknown parent", asRecipient: true, wrongKey: true, parentID: missingParent, want: errWrongKey},
		{name: "retired actor and wrong capability", asRecipient: true, wrongKey: true, retire: retireReplier, want: errRetired},
		{name: "unknown parent", asRecipient: true, parentID: missingParent, code: "not_found"},
		{name: "not the recipient and a notice", parent: asNotice, want: errNotBound},
		{name: "not the recipient and not sent", parent: notSent, want: errNotBound},
		{name: "a notice that was not sent", asRecipient: true, parent: noticeNotSent, want: "invalid_input: this is an automatic notice; use send_message to PARENT_FROM"},
		{name: "a message that was not sent", asRecipient: true, parent: notSent, want: errNotSent},
		{name: "not sent and the destination is retired", asRecipient: true, parent: notSent, retire: retireAsker, want: errNotSent},
	}
}

// replyAs sends the reply for one case and returns the error.
func replyAs(t *testing.T, f replyFixture, test requestCase) error {
	t.Helper()
	actor, key := f.replier.ID, f.replier.Key
	if !test.asRecipient {
		// Another registered agent: it authenticates, but is not the recipient.
		other := register(t, f.store, "beta")
		actor, key = other.ID, other.Key
	}
	if test.wrongKey {
		key = "not the capability"
	}
	id := f.parent.ID
	if test.parentID != "" {
		id = test.parentID
	}
	before := snapshot(t, f.store.Dir)
	restarted := restart(t, f.store)
	_, err := (&bus.Service{Store: restarted}).Reply(context.Background(), bus.ReplyRequest{Actor: actor, Capability: key, MessageID: id, Body: "pong"})
	if after := snapshot(t, f.store.Dir); !maps.Equal(before, after) {
		t.Fatalf("a refused reply changed the journal:\nbefore %v\nafter  %v", keys(before), keys(after))
	}
	return err
}

func TestReplyRequestWithSeveralFaultsReportsTheFirstCheck(t *testing.T) {
	for _, test := range requestCases() {
		t.Run(test.name, func(t *testing.T) {
			f := newReplyFixture(t)
			if test.parent != nil {
				parentOf(t, f, test.parent)
			}
			if test.retire != nil {
				test.retire(t, f)
			}
			err := replyAs(t, f, test)
			want := strings.ReplaceAll(test.want, "PARENT_FROM", f.parent.From)
			switch {
			case test.code != "" && model.ErrorCodeOf(err) != test.code:
				t.Fatalf("error code = %q (%v), want %q", model.ErrorCodeOf(err), err, test.code)
			case test.code == "" && (err == nil || err.Error() != want):
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}
