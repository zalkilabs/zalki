package pluginfeed

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxMaliciousHintIsNeverAPath(t *testing.T) {
	for _, raw := range []string{`null`, `"../x"`, `"../../outside"`, `"Uppercase"`, `""`, strings.Repeat("x", 129), `123`, `["external"]`, `{"name":"external"}`, `"` + strings.Repeat("a", 33) + `"`} {
		t.Run(raw[:min(len(raw), 20)], func(t *testing.T) {
			checkMaliciousInboxHint(t, raw)
		})
	}
}

func TestPluginInboxPrunedSenderDoesNotBrickNextSender(t *testing.T) {
	x := newInboxStorageFixture(t)
	b, key := register(t, x.f.store, "cursor")
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.f.GrantPluginCapabilities(PluginCapabilityGrants{InboxFrom: []string{x.from.ID, b.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	r := x.request()
	if _, err := x.service.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := x.f.store.Update(x.from.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.store.PruneRetired(false, nil, 0); err != nil {
		t.Fatal(err)
	}
	path, _ := sessionTestPath(x.f.store, x.from.ID)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sender was not actually pruned", err)
	}
	r.Actor, r.Capability, r.MessageID = b.ID, key, model.NewUUID()
	if _, err := x.service.Send(context.Background(), r); err != nil {
		t.Fatal("pruned historical sender bricked inbox", err)
	}
	index := x.index(t)
	if len(index.Entries) != 2 || index.Entries[0].Sender != x.from.ID || index.Entries[1].Sender != b.ID {
		t.Fatal("historical sender provenance was discarded")
	}
}

func TestPluginInboxNativeBackgroundPathsPreserveStoredBytes(t *testing.T) {
	x := newInboxStorageFixture(t)
	r := x.request()
	m, err := x.service.Send(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	// Age a real admitted record to exercise retention boundaries; no synthetic
	// submitted record is substituted for the admission operation.
	old := time.Now().Add(-31 * 24 * time.Hour)
	m.CreatedAt = old.Format(time.RFC3339Nano)
	m.Receipt.At = m.CreatedAt
	if err = saveTestMessage(x.f.store, m); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(x.f.store.Dir, "messages", m.ID+".json")
	canonical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	index := x.index(t)
	index.Entries[0].At = old
	index.Entries[0].Bytes = len(canonical)
	x.putIndex(t, index)
	indexPath := filepath.Join(x.f.pluginDataPath(), "inbox.json")
	indexBytes, _ := os.ReadFile(indexPath)
	runNativePathsAgainstPluginRecord(t, x, r)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(canonical, after) {
		t.Fatal("native background path rewrote/deleted plugin message", err)
	}
	after, err = os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(indexBytes, after) {
		t.Fatal("native background path altered plugin index/quota", err)
	}
}

func checkMaliciousInboxHint(t *testing.T, raw string) {
	t.Helper()

	x := newInboxStorageFixture(t)
	r := x.request()
	hint := filepath.Join(x.f.pluginDir(), "id-"+x.status.ID+".json")
	outside, sentinel := makeInboxHintTrap(t, x, raw)
	if err := os.WriteFile(outside, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hint, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := x.service.Send(context.Background(), r)
	assertInboxFailure(t, result, err)
	if err.Error() != "unauthorized: plugin inbox unavailable" {
		t.Fatal("hint leaked lookup detail", err)
	}
	x.assertAbsent(t, r.MessageID)
	after, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(after, sentinel) {
		t.Fatal("hint touched outside plugin namespace", err)
	}
	if raw == `"../x"` {
		entries, readErr := os.ReadDir(filepath.Join(x.f.store.Dir, "x.data"))
		if readErr != nil || len(entries) != 0 {
			t.Fatal("hint wrote outside plugin namespace", readErr)
		}
	} else if _, err = os.Stat(filepath.Join(x.f.store.Dir, "x.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("hint created an outside record")
	}

}

func makeInboxHintTrap(t *testing.T, x inboxStorageFixture, raw string) (string, []byte) {
	t.Helper()
	outside := filepath.Join(x.f.store.Dir, "outside.json")
	sentinel := []byte(`{"sentinel":"unchanged"}`)
	if raw == `"../x"` {
		p, _, loadErr := x.f.loadPlugin()
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		p.Name = "../x"
		var encodeErr error
		sentinel, encodeErr = journal.EncodeJSON(p, 16<<10)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		outside = filepath.Join(x.f.store.Dir, "x.json")
		if mkdirErr := os.Mkdir(filepath.Join(x.f.store.Dir, "x.data"), 0700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	}
	return outside, sentinel
}

func runNativePathsAgainstPluginRecord(t *testing.T, x inboxStorageFixture, r bus.SendRequest) {
	t.Helper()
	var err error
	b, _ := register(t, x.f.store, "cursor")
	var key string
	if text, err := x.f.store.EmitPending(b.ID); err != nil || strings.Contains(text, r.Body) {
		t.Fatal("native pending scan emitted plugin body", err)
	}
	if grant, err := claimCursorRecovery(x.f.store, b.ID); err != nil || grant.Capability == "" {
		t.Fatal("native recovery control did not run", err)
	} else {
		key = grant.Capability
	}
	if _, err = x.f.store.QueryMessages(bus.MessageQuery{Actor: b.ID, Capability: key, History: true, IncludeBody: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = x.service.Watch(b.ID, key, r.To); err == nil {
		t.Fatal("native watch accepted plugin")
	}
	native := bus.Service{Store: x.f.store}
	if _, err = native.Send(t.Context(), bus.SendRequest{Actor: b.ID, Capability: key, To: x.from.ID, MessageID: model.NewUUID(), Body: "native quota control"}); err != nil {
		t.Fatal(err)
	}
	if err = bus.WithJournalRead(x.f.store, func(bus.JournalView) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = x.f.store.PruneRetired(false, nil, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err = x.f.store.Update(x.from.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if _, err = x.f.store.PruneRetired(false, nil, 0); err != nil {
		t.Fatal(err)
	}
}
