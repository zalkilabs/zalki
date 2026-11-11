package bus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// One unreadable message file must not break list_messages (inbox or history).
// The caller learns only a count; exact reads and the cursor anchor stay strict.

type listFixture struct {
	store  Store
	sender model.Session
	key    string
}

// recipient registers a cursor session that receives hook-pending messages.
func (l listFixture) recipient(t *testing.T) (emitFixture, string) {
	t.Helper()
	target, key := register(t, l.store, "cursor")
	return emitFixture{store: l.store, target: target, sender: l.sender, key: l.key}, key
}

func newListFixture(t *testing.T) listFixture {
	t.Helper()
	store := fixtureStore(t)
	sender, key := register(t, store, "codex")
	return listFixture{store: store, sender: sender, key: key}
}

func readList(t *testing.T, store Store, query MessageQuery) MessageBatch {
	t.Helper()
	batch, err := store.ReadMessages(query)
	if err != nil {
		t.Fatalf("list failed on an unreadable file: %v", err)
	}
	return batch
}

func batchJSON(t *testing.T, batch MessageBatch) string {
	t.Helper()
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Investigator case: A sends to B and to C; C's file breaks. B's inbox and
// history still work and never show C's message; C sees only the count.
func TestListSkipsAnotherRecipientsUnreadableFile(t *testing.T) {
	for _, p := range []unreadableFile{truncatedMessage, modeZeroMessage} {
		t.Run(p.name, func(t *testing.T) {
			l := newListFixture(t)
			b, keyB := l.recipient(t)
			c, keyC := l.recipient(t)
			base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			toC := c.queue(t, "BODY-FOR-C", base)
			toB := b.queue(t, "BODY-FOR-B", base.Add(time.Second))
			writeUnreadableMessage(t, l.store, toC.ID, p)
			for _, history := range []bool{false, true} {
				requireOnlyOwnMessage(t, l.store, MessageQuery{Actor: b.target.ID, Capability: keyB, History: history, IncludeBody: true}, toB.ID, toC.ID)
			}
			batch := readList(t, l.store, MessageQuery{Actor: c.target.ID, Capability: keyC})
			if len(batch.Messages) != 0 || batch.Unreadable != 1 || strings.Contains(batchJSON(t, batch), toC.ID) {
				t.Fatalf("C's list = %+v", batch)
			}
			if _, err := l.store.ReadMessages(MessageQuery{Actor: c.target.ID, Capability: keyC, MessageID: toC.ID}); err == nil {
				t.Fatal("exact read of an unreadable message succeeded")
			}
		})
	}
}

// requireOnlyOwnMessage checks that a list holds exactly own, counts one
// unreadable file, and never shows the foreign message or its body.
func requireOnlyOwnMessage(t *testing.T, store Store, query MessageQuery, own, foreign string) {
	t.Helper()
	batch := readList(t, store, query)
	if len(batch.Messages) != 1 || batch.Messages[0].ID != own || batch.Unreadable != 1 {
		t.Fatalf("history=%t: list = %+v", query.History, batch)
	}
	if text := batchJSON(t, batch); strings.Contains(text, foreign) || strings.Contains(text, "BODY-FOR-C") {
		t.Fatalf("history=%t: list leaked the foreign message: %s", query.History, text)
	}
}

// pageIDs reads every page (limit 2) and returns each page's IDs and More flag.
func pageIDs(t *testing.T, store Store, actor, key string, history bool, unreadable int) []string {
	t.Helper()
	var pages []string
	cursor := ""
	for {
		batch := readList(t, store, MessageQuery{Actor: actor, Capability: key, History: history, Limit: 2, Cursor: cursor})
		if batch.Unreadable != unreadable {
			t.Fatalf("page %d: unreadable = %d, want %d", len(pages), batch.Unreadable, unreadable)
		}
		ids := make([]string, len(batch.Messages))
		for i, msg := range batch.Messages {
			ids[i] = msg.ID
		}
		pages = append(pages, strings.Join(ids, ",")+map[bool]string{true: "+more", false: ""}[batch.More])
		if !batch.More {
			return pages
		}
		cursor = batch.Messages[len(batch.Messages)-1].ID
	}
}

// Paging, order and More are the same as without the bad file, wherever the bad
// file sorts in the directory (eachMessage reads files in lexical order).
func TestListPagingIgnoresAnUnreadableFileBeforeInsideOrAfterThePage(t *testing.T) {
	for _, history := range []bool{false, true} {
		l := newListFixture(t)
		b, key := l.recipient(t)
		base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
		names := make([]string, 0, 5)
		for i := range 5 {
			names = append(names, b.queue(t, "page body", base.Add(time.Duration(i)*time.Second)).ID)
		}
		golden := pageIDs(t, l.store, b.target.ID, key, history, 0)
		slices.Sort(names)
		for _, place := range []struct{ where, name string }{
			{"before", "00000000-0000-4000-8000-000000000000"},
			{"inside", names[2] + "0"},
			{"after", "ffffffff-ffff-4fff-bfff-ffffffffffff"},
		} {
			path := filepath.Join(l.store.Dir, "messages", place.name+".json")
			if err := os.WriteFile(path, []byte(`{"schema":1,"id":"`), 0600); err != nil {
				t.Fatal(err)
			}
			if got := pageIDs(t, l.store, b.target.ID, key, history, 1); !slices.Equal(got, golden) {
				t.Fatalf("history=%t poison %s: pages %v, want %v", history, place.where, got, golden)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The cursor anchor and an exact read stay strict.
func TestListCursorAnchorAndExactReadOfAnUnreadableFileStillFail(t *testing.T) {
	l := newListFixture(t)
	b, key := l.recipient(t)
	msg := b.queue(t, "anchor", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	writeUnreadableMessage(t, l.store, msg.ID, truncatedMessage)
	if _, err := l.store.ReadMessages(MessageQuery{Actor: b.target.ID, Capability: key, Cursor: msg.ID}); err == nil {
		t.Fatal("an unreadable cursor anchor was accepted")
	}
	if _, err := l.store.ReadMessages(MessageQuery{Actor: b.target.ID, Capability: key, MessageID: msg.ID}); err == nil {
		t.Fatal("an exact read of an unreadable file succeeded")
	}
}

// The poison persists; every list call reports it once, by count and first UUID.
func TestListReportsAnUnreadableFileOncePerCall(t *testing.T) {
	l := newListFixture(t)
	b, key := l.recipient(t)
	output := capture(&l.store)
	bad := newUnreadableMessage(t, l.store, truncatedMessage)
	for call := range 2 {
		output.Reset()
		if batch := readList(t, l.store, MessageQuery{Actor: b.target.ID, Capability: key}); batch.Unreadable != 1 {
			t.Fatalf("call %d: unreadable = %d", call, batch.Unreadable)
		}
		requireOneSkipRecord(t, output, b.target.ID, bad, 1)
	}
}

// Without a bad file the batch is unchanged: no count.
func TestListWithoutUnreadableFilesHasNoCount(t *testing.T) {
	l := newListFixture(t)
	b, key := l.recipient(t)
	b.queue(t, "healthy", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	if batch := readList(t, l.store, MessageQuery{Actor: b.target.ID, Capability: key}); batch.Unreadable != 0 || len(batch.Messages) != 1 {
		t.Fatalf("healthy batch = %+v", batch)
	}
}
