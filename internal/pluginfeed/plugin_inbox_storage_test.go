package pluginfeed

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type inboxStorageFixture struct {
	f       PluginFeed
	service bus.Service
	from    model.Session
	key     string
	status  PluginFeedStatus
}

func newInboxStorageFixture(t *testing.T) inboxStorageFixture {
	t.Helper()
	f, _, _ := externalPinFixture(t)
	from, key := register(t, f.store, "cursor")
	if err := f.GrantPluginCapabilities(PluginCapabilityGrants{Receive: []string{from.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: f.store, PluginInbox: AdmitPluginInbox, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		t.Error("plugin called native transport")
		return model.NewReceipt(model.StatusSubmitted)
	}}
	return inboxStorageFixture{f, service, from, key, status}
}
func (x inboxStorageFixture) request() bus.SendRequest {
	return bus.SendRequest{Actor: x.from.ID, Capability: x.key, To: "plugin:" + x.status.ID, MessageID: model.NewUUID(), Body: "STORAGE-PRIVATE-PAYLOAD"}
}
func (x inboxStorageFixture) index(t *testing.T) pluginInboxIndex {
	t.Helper()
	raw, err := journal.ReadBytes(filepath.Join(x.f.pluginDataPath(), "inbox.json"), 1<<20)
	var out pluginInboxIndex
	if err != nil || bus.DecodeStrictRecordJSON(raw, &out) != nil {
		t.Fatal("missing valid index", err)
	}
	return out
}
func (x inboxStorageFixture) putIndex(t *testing.T, index pluginInboxIndex) {
	t.Helper()
	raw, err := journal.EncodeJSON(index, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = journal.WriteAtomic(filepath.Join(x.f.pluginDataPath(), "inbox.json"), raw); err != nil {
		t.Fatal(err)
	}
}
func (x inboxStorageFixture) assertAbsent(t *testing.T, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(x.f.store.Dir, "messages", id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canonical published after refusal", err)
	}
}
func assertInboxFailure(t *testing.T, m model.Message, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(m, model.Message{}) {
		t.Fatal("failed admission returned message data", err)
	}
}

func TestPluginInboxAdmissionRequiresCurrentNativeGrant(t *testing.T) {
	for _, name := range []string{"wrong-key", "retired", "generation", "disabled", "no-capabilities", "read-only", "foreign-installation", "replaced-installation", "foreign-pin", "ungranted", "missing-hint", "stale-hint"} {
		t.Run(name, func(t *testing.T) {
			checkInboxAdmissionGrant(t, name)
		})
	}
}

func TestPluginInboxStoredProvenanceAndExactRetry(t *testing.T) {
	x := newInboxStorageFixture(t)
	r := x.request()
	m, err := x.service.Send(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(x.f.store.Dir, "messages", m.ID+".json")
	canonical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	index, indexPath, before := checkStoredInboxProvenance(t, x, r, m, canonical)
	if err = x.f.RevokePluginCapabilities(); err != nil {
		t.Fatal(err)
	}
	again, err := x.service.Send(context.Background(), r)
	after, _ := os.ReadFile(indexPath)
	if err != nil || !reflect.DeepEqual(m, again) || !bytes.Equal(before, after) {
		t.Fatal("known UUID re-admitted or spent quota", err)
	}
	changed := r
	changed.Body = "different"
	result, err := x.service.Send(context.Background(), changed)
	assertInboxFailure(t, result, err)
	if model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("conflicting UUID accepted", err)
	}
	index.Entries[0].At = time.Now().Add(-31 * 24 * time.Hour)
	x.putIndex(t, index)
	expired, _ := os.ReadFile(indexPath)
	again, err = x.service.Send(context.Background(), r)
	after, _ = os.ReadFile(indexPath)
	if err != nil || again.ID != m.ID || !bytes.Equal(expired, after) {
		t.Fatal("retry recreated expired availability", err)
	}
	if err = x.f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	again, err = x.service.Send(context.Background(), r)
	if err != nil || again.ID != m.ID {
		t.Fatal("canonical retry depended on removed installation", err)
	}
	if _, err = os.Stat(indexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retry recreated removed inbox")
	}
}

func TestPluginInboxPublicationCrashOrder(t *testing.T) {
	for _, cut := range []string{"index-precommit", "index-post-rename", "canonical-dir-sync"} {
		t.Run(cut, func(t *testing.T) {
			checkInboxPublicationCut(t, cut)
		})
	}
}

func TestPluginInboxCountAndFullEncodedByteQuota(t *testing.T) {
	for _, scope := range []string{"count", "bytes", "full-encoding"} {
		t.Run(scope, func(t *testing.T) {
			checkInboxCapacity(t, scope)
		})
	}
}

func TestPluginInboxCorruptIndexFailsClosed(t *testing.T) {
	for _, kind := range []string{"version", "duplicate", "foreign", "generation", "revision", "negative-size", "future-entry", "future-quota", "unknown-field", "oversize", "symlink", "flood"} {
		t.Run(kind, func(t *testing.T) {
			checkInboxCorruption(t, kind)
		})
	}
}

func TestPluginInboxExistingInstallationHintAndRemoval(t *testing.T) {
	x := newInboxStorageFixture(t)
	hint := filepath.Join(x.f.pluginDir(), "id-"+x.status.ID+".json")
	before, _ := os.ReadFile(x.f.pluginPath())
	if err := os.Remove(hint); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(x.f.pluginPath())
	if !bytes.Equal(before, after) {
		t.Fatal("backfill changed installation/grants/feed receipt")
	}
	r := x.request()
	if _, err := x.service.Send(context.Background(), r); err != nil {
		t.Fatal("backfilled identity did not route", err)
	}
	if err := x.f.RevokePluginCapabilities(); err != nil {
		t.Fatal(err)
	}
	if err := x.f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{x.f.pluginPath(), hint, x.f.pluginDataPath()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("removal retained installation namespace", path, err)
		}
	}
	r.MessageID = model.NewUUID()
	m, err := x.service.Send(context.Background(), r)
	assertInboxFailure(t, m, err)
	x.assertAbsent(t, r.MessageID)
}

func checkInboxAdmissionGrant(t *testing.T, name string) {
	t.Helper()

	x := newInboxStorageFixture(t)
	r := x.request()
	p, _, err := x.f.loadPlugin()
	if err != nil {
		t.Fatal(err)
	}
	mutateInboxNativePrincipal(t, x, &r, name)
	mutateInboxCapabilities(&p, x, name)
	mutateInboxHint(t, x, p, name)
	if _, err = x.f.savePlugin(p); err != nil {
		t.Fatal(err)
	}
	m, err := x.service.Send(context.Background(), r)
	assertInboxFailure(t, m, err)
	x.assertAbsent(t, r.MessageID)
	if strings.Contains(err.Error(), r.Body) || strings.Contains(err.Error(), r.Capability) {
		t.Fatal("failure leaked body or key")
	}

}

func checkInboxPublicationCut(t *testing.T, cut string) {
	t.Helper()

	x := newInboxStorageFixture(t)
	r := x.request()
	indexPath := filepath.Join(x.f.pluginDataPath(), "inbox.json")
	prepareInboxPublicationCut(t, x, cut, indexPath)
	m, err := x.service.Send(context.Background(), r)
	assertInboxFailure(t, m, err)
	if cut == "index-precommit" {
		x.assertAbsent(t, r.MessageID)
		return
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal("cut was not directory sync", err)
	}
	if err = os.Chmod(x.f.pluginDataPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Join(x.f.store.Dir, "messages"), 0700); err != nil {
		t.Fatal(err)
	}
	index := x.index(t)
	if len(index.Entries) != 1 || index.Entries[0].ID != r.MessageID || len(index.Quota.Recent) != 1 {
		t.Fatal("post-rename index/quota reservation absent")
	}
	before, _ := os.ReadFile(indexPath)
	fresh := bus.Service{Store: bus.Store{Dir: x.f.store.Dir, Registry: testRegistry()}, PluginInbox: AdmitPluginInbox}
	again, retryErr := fresh.Send(context.Background(), r)
	after, _ := os.ReadFile(indexPath)
	if !bytes.Equal(before, after) {
		t.Fatal("crash retry spent quota or republished index")
	}
	// The canonical message is committed with its event in one batch that
	// orders messages/ first, so a directory that cannot be synced stops the
	// write before the rename: both cuts leave a reservation and no message,
	// and the retry fails closed.
	assertInboxFailure(t, again, retryErr)
	if !strings.Contains(retryErr.Error(), "send again with a new message_id") {
		t.Fatal("the retry error does not tell the sender what to do", retryErr)
	}
	x.assertAbsent(t, r.MessageID)

}

func checkInboxCapacity(t *testing.T, scope string) {
	t.Helper()

	x := newInboxStorageFixture(t)
	initial := x.request()
	if _, err := x.service.Send(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	index := x.index(t)
	index.Entries = nil
	now := time.Now().UTC()
	count, size := 1000, 1024
	if scope != "count" {
		count, size = 128, maxStateFileBytes
	}
	for range count {
		index.Entries = append(index.Entries, pluginInboxEntry{ID: model.NewUUID(), Sender: x.from.ID, Generation: x.from.Generation, Revision: x.status.Capabilities.Revision, At: now.Add(-2 * time.Hour), Bytes: size})
	}
	if scope == "full-encoding" {
		index.Entries[len(index.Entries)-1].Bytes -= 12 << 10
	}
	index.Quota = model.NewFloodState(now)
	x.putIndex(t, index)
	path := filepath.Join(x.f.pluginDataPath(), "inbox.json")
	before, _ := os.ReadFile(path)
	r := x.request()
	if scope == "full-encoding" {
		r.Body = strings.Repeat("<", 8<<10)
	}
	m, err := x.service.Send(context.Background(), r)
	assertInboxFailure(t, m, err)
	if model.ErrorCodeOf(err) != model.CodeRateLimited {
		t.Fatal("bounded inbox did not refuse quota", scope, err)
	}
	x.assertAbsent(t, r.MessageID)
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("refusal changed quota/index")
	}

}

func checkInboxCorruption(t *testing.T, kind string) {
	t.Helper()

	x := newInboxStorageFixture(t)
	r := x.request()
	if _, err := x.service.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	index := x.index(t)
	path := filepath.Join(x.f.pluginDataPath(), "inbox.json")
	poisonInboxMetadata(&index, kind)
	x.putIndex(t, index)
	if kind == "unknown-field" {
		raw := []byte(`{"version":1,"installation":"` + x.status.ID + `","unknown":true}`)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "oversize" {
		if err := os.WriteFile(path, []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "symlink" {
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.Rename(path, outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(path)
	next := x.request()
	m, err := x.service.Send(context.Background(), next)
	assertInboxFailure(t, m, err)
	x.assertAbsent(t, next.MessageID)
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("corrupt/flood refusal overwrote state")
	}
	if kind == "flood" && model.ErrorCodeOf(err) != model.CodeRateLimited {
		t.Fatal("flood refusal bypassed", err)
	}

}

func checkStoredInboxProvenance(t *testing.T, x inboxStorageFixture, r bus.SendRequest, m model.Message, canonical []byte) (pluginInboxIndex, string, []byte) {
	t.Helper()
	index := x.index(t)
	if index.Installation != x.status.ID || len(index.Entries) != 1 || len(index.Quota.Recent) != 1 {
		t.Fatal("storage or quota provenance missing")
	}
	e := index.Entries[0]
	if !reflect.DeepEqual([]any{e.ID, e.Sender, e.Generation, e.Revision, e.Bytes, e.At.Format(time.RFC3339Nano)}, []any{r.MessageID, x.from.ID, x.from.Generation, x.status.Capabilities.Revision, len(canonical), m.CreatedAt}) {
		t.Fatal("caller-controlled or incomplete provenance")
	}
	indexPath := filepath.Join(x.f.pluginDataPath(), "inbox.json")
	before, _ := os.ReadFile(indexPath)
	if bytes.Contains(before, []byte(r.Body)) || bytes.Contains(before, []byte(r.Capability)) {
		t.Fatal("index contains body or native capability")
	}
	return index, indexPath, before
}

func mutateInboxNativePrincipal(t *testing.T, x inboxStorageFixture, r *bus.SendRequest, name string) {
	t.Helper()
	var err error
	switch name {
	case "wrong-key":
		r.Capability = "SYNTHETIC-INCORRECT-KEY"
	case "retired":
		if err = x.f.store.Update(x.from.ID, model.StateRetired, true); err != nil {
			t.Fatal(err)
		}
	case "generation":
		from := x.from
		from.Generation = "invalid"
		path, _ := sessionTestPath(x.f.store, from.ID)
		if err = writeJSON(path, from); err != nil {
			t.Fatal(err)
		}
	}
}

func mutateInboxCapabilities(p *pluginRecord, x inboxStorageFixture, name string) {
	switch name {
	case "disabled":
		p.Enabled = false
	case "no-capabilities":
		p.Capabilities = nil
	case "read-only":
		p.Capabilities.Receive = nil
		p.Capabilities.Read = []string{x.from.ID}
	case "foreign-installation":
		p.Capabilities.Installation = model.NewUUID()
	case "replaced-installation":
		p.ID = model.NewUUID()
		p.Capabilities.Installation = p.ID
	case "foreign-pin":
		p.Capabilities.PinSHA256 = strings.Repeat("a", 64)
	case "ungranted":
		other := x.from
		other.ID = "cursor:00000000-0000-4000-8000-000000000099"
		p.Capabilities.Receive = []string{other.ID}
	}
}

func mutateInboxHint(t *testing.T, x inboxStorageFixture, p pluginRecord, name string) {
	t.Helper()
	var err error
	switch name {
	case "missing-hint":
		if err = os.Remove(filepath.Join(x.f.pluginDir(), "id-"+p.ID+".json")); err != nil {
			t.Fatal(err)
		}
	case "stale-hint":
		raw, _ := journal.EncodeJSON("absent-installation", 128)
		if err = journal.WriteAtomic(filepath.Join(x.f.pluginDir(), "id-"+p.ID+".json"), raw); err != nil {
			t.Fatal(err)
		}
	}
}

func prepareInboxPublicationCut(t *testing.T, x inboxStorageFixture, cut, indexPath string) {
	t.Helper()
	if cut == "index-precommit" {
		if err := os.Mkdir(indexPath, 0700); err != nil {
			t.Fatal(err)
		}
	} else {
		if os.Geteuid() == 0 {
			t.Skip("directory sync permission cut requires ordinary user")
		}
		dir := x.f.pluginDataPath()
		if cut == "canonical-dir-sync" {
			dir = filepath.Join(x.f.store.Dir, "messages")
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
		if err := os.Chmod(dir, 0300); err != nil {
			t.Fatal(err)
		}
	}
}

func poisonInboxMetadata(index *pluginInboxIndex, kind string) {
	switch kind {
	case "version":
		index.Version = 2
	case "duplicate":
		index.Entries = append(index.Entries, index.Entries[0])
	case "foreign":
		index.Installation = model.NewUUID()
	case "generation":
		index.Entries[0].Generation = "bad"
	case "revision":
		index.Entries[0].Revision = "bad"
	case "negative-size":
		index.Entries[0].Bytes = -1
	case "future-entry":
		index.Entries[0].At = time.Now().Add(time.Hour)
	case "future-quota":
		index.Quota.Inbound.At = time.Now().Add(time.Hour)
	case "flood":
		index.Quota.Inbound.Tokens = 0
		index.Quota.Inbound.At = time.Now().UTC()
	}
}
