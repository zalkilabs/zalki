package bus_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// These tests pin the read path of Store.ReadMessages (scanMessages and the
// exact lookup): who may see which message, in which order, and what a failed
// read still returns. They use the exported API and the journal files only, so
// they survive later moves of the model types. Odd results are characterized,
// not endorsed.
//
// scanMessages does this, in this order, under the journal lock:
//  1. A cursor, if given, is loaded: a missing one is an error; one that is
//     neither from nor to the caller is refused ("cursor belongs to another
//     session"). Nothing is scanned after that error.
//  2. Every record is read in file-name (message ID) order. An unreadable record
//     stops the scan with its error; the views collected so far are kept.
//  3. Per record: it must involve the caller (from or to); with Sent it must be
//     sent by the caller; with To it must be to that recipient; in inbox mode
//     (no History, no Sent) it must be to the caller, for the caller's current
//     registration, unacknowledged and still acknowledgeable; with a cursor it
//     must come strictly after the cursor in display order.
//  4. The views are sorted: inbox oldest first, history newest first; equal
//     creation times are ordered by ID, in both modes.
//  5. More than the limit are cut and More is reported.
// Exact lookup (MessageID) refuses a message of another session, then a
// message not sent by the caller (with Sent), then one not sent to To.
// The caller is authenticated before any of this.

const secretBody = "SECRET-THIRD-PARTY-BODY"

// journalFixture has three agents on a journal-inbox adapter (bodies are
// visible in the inbox): A is the caller, B and C are the others.
type journalFixture struct {
	store   bus.Store
	a, b, c agent
}

func newJournalFixture(t *testing.T) journalFixture {
	t.Helper()
	store := newStore(t, listenerAdapter("alpha"), listenerAdapter("beta"), listenerAdapter("gamma"))
	return journalFixture{store: store, a: register(t, store, "alpha"), b: register(t, store, "beta"), c: register(t, store, "gamma")}
}

// put writes a submitted, unacknowledged message record directly.
func (f journalFixture) put(t *testing.T, number int, from, to agent, body string, minute int) string {
	t.Helper()
	id := messageID(number)
	writeJSONFile(t, filepath.Join(f.store.Dir, "messages", id+".json"), map[string]any{
		"schema": 1, "id": id, "from": from.ID, "to": to.ID, "body": body, "target_generation": to.Generation,
		"created_at": fmt.Sprintf("2026-11-01T12:%02d:00.000000000Z", minute), "status": "submitted",
	})
	return id
}

func messageID(number int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", number) }

// putTied writes messages with the same creation time, for tie cases.
func (f journalFixture) putTied(t *testing.T, number int, from, to agent, body string) string {
	t.Helper()
	return f.put(t, number, from, to, body, 30)
}

func (f journalFixture) read(query bus.MessageQuery) (bus.MessageBatch, error) {
	return f.store.ReadMessages(query)
}

// as fills in the caller of a query.
func as(a agent, query bus.MessageQuery) bus.MessageQuery {
	query.Actor, query.Capability = a.ID, a.Key
	return query
}

func idsOf(batch bus.MessageBatch) []string {
	ids := make([]string, len(batch.Messages))
	for i, msg := range batch.Messages {
		ids[i] = msg.ID
	}
	return ids
}

// numbers turns message IDs back into the numbers used by put.
func numbers(ids []string) []int {
	out := make([]int, len(ids))
	for i, id := range ids {
		_, _ = fmt.Sscanf(id[len(id)-12:], "%d", &out[i])
	}
	return out
}

// seedThirdParties adds messages that do not involve A, with a secret body.
func (f journalFixture) seedThirdParties(t *testing.T) {
	t.Helper()
	f.put(t, 101, f.b, f.c, secretBody+"-1", 1)
	f.put(t, 102, f.c, f.b, secretBody+"-2", 2)
	f.put(t, 103, f.c, f.b, secretBody+"-3", 40)
	f.put(t, 104, f.b, f.c, secretBody+"-4", 41)
}

// seedA adds messages that involve A: three inbound (inbox work), two outbound.
func (f journalFixture) seedA(t *testing.T) {
	t.Helper()
	f.put(t, 1, f.b, f.a, "in-1", 10)
	f.put(t, 2, f.a, f.b, "out-1", 11)
	f.put(t, 3, f.c, f.a, "in-2", 12)
	f.put(t, 4, f.a, f.c, "out-2", 13)
	f.put(t, 5, f.b, f.a, "in-3", 14)
}

// assertOnlyTheCallersMessages runs a query as A and checks that every message
// involves A and that no secret text came back.
func assertOnlyTheCallersMessages(t *testing.T, f journalFixture, query bus.MessageQuery) {
	t.Helper()
	batch, err := f.read(as(f.a, query))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Messages) == 0 {
		t.Fatal("the query returned nothing, so it proves nothing")
	}
	for _, msg := range batch.Messages {
		if msg.From != f.a.ID && msg.To != f.a.ID {
			t.Fatalf("returned a message between %s and %s", msg.From, msg.To)
		}
		// BodyPreview is deliberately excluded from Message's JSON encoding.
		if strings.Contains(msg.Body, secretBody) || strings.Contains(msg.BodyPreview, secretBody) {
			t.Fatalf("third-party text reached the caller's body or preview: %+v", msg)
		}
		if query.Preview && msg.BodyPreview == "" {
			t.Fatal("the sent-preview query did not exercise the preview field")
		}
	}
	encoded, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET") {
		t.Fatalf("a third party's text reached the caller: %s", encoded)
	}
}

func TestReadMessagesNeverReturnsAThirdPartysMessage(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.seedA(t)
	queries := map[string]bus.MessageQuery{
		"inbox":                       {},
		"inbox with a cursor":         {Cursor: messageID(1)},
		"history":                     {History: true},
		"history with bodies":         {History: true, IncludeBody: true},
		"history with a cursor":       {History: true, IncludeBody: true, Cursor: messageID(5)},
		"sent":                        {Sent: true},
		"sent with bodies":            {Sent: true, IncludeBody: true},
		"sent to another agent":       {Sent: true, To: f.c.ID, IncludeBody: true},
		"sent with a preview":         {Sent: true, Preview: true},
		"history at the largest page": {History: true, IncludeBody: true, Limit: 100},
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) { assertOnlyTheCallersMessages(t, f, query) })
	}
	// The same journal does hold the secrets: the agents they belong to see them.
	seen, err := f.read(as(f.c, bus.MessageQuery{History: true, IncludeBody: true, Limit: 100}))
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(seen); !strings.Contains(string(encoded), secretBody) {
		t.Fatal("setup: the third-party messages are not in the journal")
	}
}

func TestReadMessagesRefusesAMessageOfAnotherSessionByIDOrByCursor(t *testing.T) {
	const (
		foreignCursor = "unauthorized: cursor belongs to another session"
		foreignExact  = "unauthorized: message belongs to another session"
		notSentByYou  = "not_found: message was not sent by this session"
		notToThem     = "not_found: message was not sent to this recipient"
	)
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.seedA(t)
	foreign, received, sentToB := messageID(101), messageID(1), messageID(2)
	tests := []struct {
		name  string
		query bus.MessageQuery
		want  string
	}{
		{"cursor in inbox mode", bus.MessageQuery{Cursor: foreign}, foreignCursor},
		{"cursor in history mode", bus.MessageQuery{History: true, Cursor: foreign}, foreignCursor},
		{"cursor in sent mode", bus.MessageQuery{Sent: true, Cursor: foreign}, foreignCursor},
		{"exact lookup", bus.MessageQuery{MessageID: foreign}, foreignExact},
		{"exact lookup of a received message with sent", bus.MessageQuery{MessageID: received, Sent: true}, notSentByYou},
		{"exact lookup sent to someone else", bus.MessageQuery{MessageID: sentToB, Sent: true, To: f.c.ID}, notToThem},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch, err := f.read(as(f.a, test.query))
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if batch.Messages == nil || len(batch.Messages) != 0 || batch.More {
				t.Fatalf("a refused read returned %#v, want an empty non-nil view", batch)
			}
		})
	}
}

// malformed writes a message file that cannot be read.
func (f journalFixture) malformed(t *testing.T, number int) {
	t.Helper()
	path := filepath.Join(f.store.Dir, "messages", messageID(number)+".json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadMessagesChecksTheCallerThenTheCursorBeforeScanning(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.seedA(t)
	f.malformed(t, 50)
	t.Run("a foreign cursor wins over an unreadable record", func(t *testing.T) {
		batch, err := f.read(as(f.a, bus.MessageQuery{Cursor: messageID(101)}))
		if err == nil || err.Error() != "unauthorized: cursor belongs to another session" {
			t.Fatalf("error = %v, want the cursor refusal", err)
		}
		if batch.Messages == nil || len(batch.Messages) != 0 {
			t.Fatalf("view = %#v, want empty and non-nil", batch.Messages)
		}
	})
	t.Run("a missing cursor wins over an unreadable record", func(t *testing.T) {
		_, err := f.read(as(f.a, bus.MessageQuery{Cursor: messageID(999)}))
		if model.ErrorCodeOf(err) != "not_found" {
			t.Fatalf("error = %v, want not_found for the cursor", err)
		}
	})
	t.Run("the caller is authenticated before the cursor is looked at", func(t *testing.T) {
		query := as(f.a, bus.MessageQuery{Cursor: messageID(101)})
		query.Capability = "not the capability"
		_, err := f.read(query)
		if err == nil || err.Error() != "unauthorized: wrong session capability; use your own SessionStart context" {
			t.Fatalf("error = %v, want the capability refusal", err)
		}
	})
	t.Run("without a cursor the unreadable record is skipped and counted", func(t *testing.T) {
		if batch, err := f.read(as(f.a, bus.MessageQuery{})); err != nil || batch.Unreadable != 1 {
			t.Fatalf("read = %d unreadable, %v; want the record skipped and counted", batch.Unreadable, err)
		}
	})
}

// An unreadable record is skipped and counted. The views before and after it
// are returned in display order, as if the record were absent.
func TestReadMessagesSkipsAnUnreadableRecordAndKeepsLaterViews(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.put(t, 9, f.b, f.a, "in-middle", 19)
	f.put(t, 10, f.b, f.a, "in-newer", 20)
	f.put(t, 11, f.b, f.a, "in-oldest", 18)
	f.malformed(t, 12)
	f.put(t, 13, f.b, f.a, "in-after", 18)
	for _, test := range []struct {
		name  string
		query bus.MessageQuery
		want  []int
	}{
		{"inbox", bus.MessageQuery{}, []int{11, 13, 9, 10}},
		{"history", bus.MessageQuery{History: true}, []int{10, 9, 11, 13}},
		{"sent", bus.MessageQuery{Sent: true}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			batch, err := f.read(as(f.a, test.query))
			if err != nil || batch.Unreadable != 1 || batch.More {
				t.Fatalf("read = %+v, %v; want the record skipped and counted", batch, err)
			}
			var got []int
			for _, n := range numbers(idsOf(batch)) {
				if n >= 9 && n <= 13 {
					got = append(got, n)
				}
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("views = %v, want %v (display order)", got, test.want)
			}
		})
	}
}

func TestReadMessagesOrdersByCreationTimeAndStopsAtTheLimit(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.seedA(t)
	// A is involved in 1..5: inbox 1, 3, 5 (oldest first); history 5, 4, 3, 2, 1.
	tests := []struct {
		name  string
		query bus.MessageQuery
		want  []int
		more  bool
	}{
		{"inbox is oldest first", bus.MessageQuery{}, []int{1, 3, 5}, false},
		{"inbox limit equals the matches", bus.MessageQuery{Limit: 3}, []int{1, 3, 5}, false},
		{"inbox limit one below the matches", bus.MessageQuery{Limit: 2}, []int{1, 3}, true},
		{"history is newest first", bus.MessageQuery{History: true}, []int{5, 4, 3, 2, 1}, false},
		{"history limit equals the matches", bus.MessageQuery{History: true, Limit: 5}, []int{5, 4, 3, 2, 1}, false},
		{"history limit one below the matches", bus.MessageQuery{History: true, Limit: 4}, []int{5, 4, 3, 2}, true},
		{"history limit above the matches", bus.MessageQuery{History: true, Limit: 6}, []int{5, 4, 3, 2, 1}, false},
		{"sent is newest first", bus.MessageQuery{Sent: true}, []int{4, 2}, false},
		{"the default limit is five", bus.MessageQuery{History: true, Limit: 0}, []int{5, 4, 3, 2, 1}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch, err := f.read(as(f.a, test.query))
			if err != nil {
				t.Fatal(err)
			}
			if got := numbers(idsOf(batch)); !slices.Equal(got, test.want) || batch.More != test.more {
				t.Fatalf("got %v more=%v, want %v more=%v", got, batch.More, test.want, test.more)
			}
		})
	}
	for _, limit := range []int{-1, 101} {
		if _, err := f.read(as(f.a, bus.MessageQuery{Limit: limit})); err == nil || err.Error() != "invalid_input: limit must be 1..100" {
			t.Fatalf("limit %d: error = %v", limit, err)
		}
	}
}

func TestReadMessagesDefaultLimitIsFiveWhenMoreMessagesMatch(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.seedA(t)
	f.put(t, 6, f.b, f.a, "sixth matching message", 15)
	for _, test := range []struct {
		name  string
		limit int
		want  []int
		more  bool
	}{
		{"default stops at five", 0, []int{6, 5, 4, 3, 2}, true},
		{"explicit six returns every match", 6, []int{6, 5, 4, 3, 2, 1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			batch, err := f.read(as(f.a, bus.MessageQuery{History: true, Limit: test.limit}))
			if err != nil {
				t.Fatal(err)
			}
			if got := numbers(idsOf(batch)); !slices.Equal(got, test.want) || batch.More != test.more {
				t.Fatalf("default-limit view = %v more=%v, want %v more=%v", got, batch.More, test.want, test.more)
			}
		})
	}
}

func TestReadMessagesOrdersEqualCreationTimesByIDInBothModes(t *testing.T) {
	f := newJournalFixture(t)
	// Written out of ID order; all created in the same instant.
	for _, number := range []int{3, 1, 2} {
		f.putTied(t, number, f.b, f.a, fmt.Sprintf("tied-%d", number))
	}
	for _, query := range []bus.MessageQuery{{}, {History: true}} {
		batch, err := f.read(as(f.a, query))
		if err != nil {
			t.Fatal(err)
		}
		if got := numbers(idsOf(batch)); !slices.Equal(got, []int{1, 2, 3}) {
			t.Fatalf("history=%v: tied messages came in order %v, want ID order 1 2 3", query.History, got)
		}
	}
}

func TestReadMessagesCursorMeansStrictlyAfterInDisplayOrder(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.seedA(t)
	tests := []struct {
		name  string
		query bus.MessageQuery
		want  []int
	}{
		{"inbox after the oldest", bus.MessageQuery{Cursor: messageID(1)}, []int{3, 5}},
		{"inbox after the newest", bus.MessageQuery{Cursor: messageID(5)}, nil},
		{"history after the newest", bus.MessageQuery{History: true, Cursor: messageID(5)}, []int{4, 3, 2, 1}},
		{"history after the oldest", bus.MessageQuery{History: true, Cursor: messageID(1)}, nil},
		{"history after a message that is not in the inbox", bus.MessageQuery{History: true, Cursor: messageID(4)}, []int{3, 2, 1}},
		{"sent after the newest sent", bus.MessageQuery{Sent: true, Cursor: messageID(4)}, []int{2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch, err := f.read(as(f.a, test.query))
			if err != nil {
				t.Fatal(err)
			}
			if got := numbers(idsOf(batch)); !slices.Equal(got, test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestReadMessagesPagesWithoutRepeatsOrGaps(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	// Eight messages for A, with ties in the middle of the list.
	for number, minute := range []int{10, 11, 12, 12, 12, 13, 14, 15} {
		from := f.b
		if number%2 == 1 {
			from = f.c
		}
		f.put(t, number+1, from, f.a, fmt.Sprintf("m-%d", number+1), minute)
	}
	for _, history := range []bool{false, true} {
		var seen []int
		cursor := ""
		for page := 0; page < 10; page++ {
			batch, err := f.read(as(f.a, bus.MessageQuery{History: history, Limit: 3, Cursor: cursor}))
			if err != nil {
				t.Fatal(err)
			}
			seen = append(seen, numbers(idsOf(batch))...)
			if !batch.More {
				break
			}
			if len(batch.Messages) == 0 {
				t.Fatal("pagination reported more without returning a message")
			}
			cursor = batch.Messages[len(batch.Messages)-1].ID
		}
		want := []int{1, 2, 3, 4, 5, 6, 7, 8}
		if history {
			want = []int{8, 7, 6, 3, 4, 5, 2, 1}
		}
		if !slices.Equal(seen, want) {
			t.Fatalf("history=%v: pages gave %v, want %v", history, seen, want)
		}
	}
}

// A cursor may name any message the caller sent or received, not only inbox work.
func TestReadMessagesAcceptsACursorOnAnyMessageOfTheCaller(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.seedA(t)
	for name, cursor := range map[string]string{"received": messageID(1), "sent": messageID(2)} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.read(as(f.a, bus.MessageQuery{History: true, Cursor: cursor})); err != nil {
				t.Fatalf("a cursor on a %s message was refused: %v", name, err)
			}
		})
	}
}

// edit rewrites one stored message record.
func (f journalFixture) edit(t *testing.T, number int, change func(record map[string]any)) {
	t.Helper()
	path := filepath.Join(f.store.Dir, "messages", messageID(number)+".json")
	record := readJSONFile(t, path)
	change(record)
	writeJSONFile(t, path, record)
}

// Inbox mode is open work only: a message that was acknowledged, that was never
// sent, or that was meant for an earlier registration of the caller is not
// shown, while history still lists it.
func TestReadMessagesInboxHidesWorkThatIsNotOpenButHistoryKeepsIt(t *testing.T) {
	f := newJournalFixture(t)
	f.put(t, 1, f.b, f.a, "open", 10)
	f.put(t, 2, f.b, f.a, "acknowledged", 11)
	f.put(t, 3, f.b, f.a, "never sent", 12)
	f.put(t, 4, f.b, f.a, "for an earlier registration", 13)
	f.put(t, 5, f.b, f.a, "open too", 14)
	// Sent by the caller, with a generation that would pass the test for received work.
	f.put(t, 6, f.a, f.b, "sent by the caller", 15)
	f.edit(t, 6, func(record map[string]any) { record["target_generation"] = f.a.Generation })
	f.edit(t, 2, func(record map[string]any) { record["acknowledged_at"] = "2026-11-01T12:20:00.000000000Z" })
	f.edit(t, 3, func(record map[string]any) { record["status"] = "not_sent" })
	f.edit(t, 4, func(record map[string]any) { record["target_generation"] = "11111111-1111-4111-8111-111111111111" })
	inbox, err := f.read(as(f.a, bus.MessageQuery{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(idsOf(inbox)); !slices.Equal(got, []int{1, 5}) {
		t.Fatalf("inbox = %v, want only the open work 1 and 5 (not 6, sent by the caller)", got)
	}
	history, err := f.read(as(f.a, bus.MessageQuery{History: true, Limit: 100}))
	if err != nil {
		t.Fatal(err)
	}
	if got := numbers(idsOf(history)); !slices.Equal(got, []int{6, 5, 4, 3, 2, 1}) {
		t.Fatalf("history = %v, want all six", got)
	}
}

func TestReadMessagesSentToFiltersByRecipient(t *testing.T) {
	f := newJournalFixture(t)
	f.seedThirdParties(t)
	f.seedA(t) // sent by A: 2 to B, 4 to C
	for _, test := range []struct {
		to   agent
		want []int
	}{{f.b, []int{2}}, {f.c, []int{4}}} {
		batch, err := f.read(as(f.a, bus.MessageQuery{Sent: true, To: test.to.ID}))
		if err != nil {
			t.Fatal(err)
		}
		if got := numbers(idsOf(batch)); !slices.Equal(got, test.want) {
			t.Fatalf("sent to %s = %v, want %v", test.to.ID, got, test.want)
		}
	}
}
