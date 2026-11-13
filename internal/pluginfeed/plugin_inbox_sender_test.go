package pluginfeed

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// TestInboxOffersOnlyCurrentMembers: X sends, then Y sends. With X retired,
// inbox_next skips X's message (it stays) and returns Y's, and the owner
// read refuses X's. When X registers again, X's message comes back first.
func TestInboxOffersOnlyCurrentMembers(t *testing.T) {
	x := newInboxReadFixture(t, "")
	y, yKey := register(t, x.f.store, "cursor")
	ySent, err := x.service.Send(context.Background(), sendFrom(x, y.ID, yKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := x.f.store.Update(x.from.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if m := x.next(t); m == nil || m.ID != ySent.ID {
		t.Fatal("a retired sender was offered, or the inbox stalled", m)
	}
	if len(x.index(t).Entries) != 2 {
		t.Fatal("the retired sender's entry was dropped")
	}
	if raw, err := x.f.ReadPluginInboxMessage(x.message.ID); len(raw) != 0 || err == nil {
		t.Fatal("the owner read offered a retired sender's message", err)
	}
	if _, _, err := x.f.store.Register("cursor", strings.TrimPrefix(x.from.ID, "cursor:"), "/workspace", nil); err != nil {
		t.Fatal(err)
	}
	if m := x.next(t); !x.isFirst(m) {
		t.Fatal("the member's message did not come back", m)
	}
}

// Inbox validity is by the sender's identity: a re-registered sender keeps its
// entry. A sender whose session record is gone loses it from the index, and
// the message file stays byte for byte.
func TestInboxEntriesFollowTheSenderIdentity(t *testing.T) {
	x := newInboxReadFixture(t, "")
	path := filepath.Join(x.f.store.Dir, "messages", x.message.ID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := x.f.store.Register("cursor", strings.TrimPrefix(x.from.ID, "cursor:"), "/workspace", nil)
	if err != nil || again.ID != x.from.ID || again.Generation == x.from.Generation {
		t.Fatal("re-registration did not give a new generation", again, err)
	}
	if m := x.next(t); m == nil || m.ID != x.message.ID {
		t.Fatal("a re-registered sender lost its entry", m)
	}
	if err := os.Remove(filepath.Join(x.f.store.Dir, "sessions", strings.ReplaceAll(x.from.ID, ":", "_")+".json")); err != nil {
		t.Fatal(err)
	}
	if m := x.next(t); m != nil {
		t.Fatal("an entry of a gone sender was offered", m)
	}
	if len(x.index(t).Entries) != 0 {
		t.Fatal("the dead entry stayed in the index")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("the message record changed or was deleted", err)
	}
}

// sendFrom is the fixture request, sent by another session.
func sendFrom(x inboxReadFixture, from, key string) bus.SendRequest {
	r := x.request()
	r.Actor, r.Capability, r.MessageID = from, key, model.NewUUID()
	return r
}
