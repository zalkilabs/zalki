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

// regrantInbox sets the accept_from list (disable, grant, enable) and
// returns the status of a new launch.
func regrantInbox(t *testing.T, f PluginFeed, from ...string) PluginFeedStatus {
	t.Helper()
	if err := f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := f.GrantPluginCapabilities(PluginCapabilityGrants{InboxRead: true, InboxFrom: from}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// TestInboxOffersOnlyCurrentSenders: X sends, then Y sends. With X's grant
// removed, inbox_next skips X's message (it stays) and returns Y's. With X
// granted again, X's message comes back first.
func TestInboxOffersOnlyCurrentSenders(t *testing.T) {
	x := newInboxReadFixture(t, "")
	y, yKey := register(t, x.f.store, "cursor")
	x.status = regrantInbox(t, x.f, x.from.ID, y.ID)
	ySent, err := x.service.Send(context.Background(), sendFrom(x, y.ID, yKey))
	if err != nil {
		t.Fatal(err)
	}
	x.status = regrantInbox(t, x.f, y.ID)
	if m := x.next(t); m == nil || m.ID != ySent.ID {
		t.Fatal("a sender without the grant was offered, or the inbox stalled", m)
	}
	if len(x.index(t).Entries) != 2 {
		t.Fatal("the ungranted sender's entry was dropped")
	}
	if raw, err := x.f.ReadPluginInboxMessage(x.message.ID); len(raw) != 0 || err == nil {
		t.Fatal("the owner read offered an ungranted sender's message", err)
	}
	x.status = regrantInbox(t, x.f, x.from.ID, y.ID)
	if m := x.next(t); !x.isFirst(m) {
		t.Fatal("the re-granted sender's message did not come back", m)
	}
}

// TestInboxDropsEntriesOfAReplacedRegistration: X re-registers, so its old
// entry can never be offered. It leaves the index; the message file stays
// byte for byte.
func TestInboxDropsEntriesOfAReplacedRegistration(t *testing.T) {
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
	if m := x.next(t); m != nil {
		t.Fatal("an entry of a replaced registration was offered", m)
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
