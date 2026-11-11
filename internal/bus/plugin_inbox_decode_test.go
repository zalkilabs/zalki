package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPluginInboxExistingIndexNeverBorrowsFreshDefaults(t *testing.T) {
	for _, kind := range []string{"null", "empty-object", "missing-version", "missing-installation", "missing-quota", "empty-quota", "missing-entries", "null-entries"} {
		t.Run(kind, func(t *testing.T) { checkExistingInboxIndexFields(t, kind) })
	}
}
func checkExistingInboxIndexFields(t *testing.T, kind string) {
	t.Helper()
	x := newInboxStorageFixture(t)
	seed := x.request()
	if _, err := x.service.Send(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(x.f.pluginDataPath(), "inbox.json")
	canonicalPath := filepath.Join(x.f.store.Dir, "messages", seed.MessageID+".json")
	canonical, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "null":
		raw = []byte(`null`)
	case "empty-object":
		raw = []byte(`{}`)
	default:
		poisonExistingIndexField(fields, kind)
		raw, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(indexPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before := journalSnapshot(t, x.f.store)
	request := x.request()
	m, err := x.service.Send(context.Background(), request)
	assertInboxFailure(t, m, err)
	if !reflect.DeepEqual(before, journalSnapshot(t, x.f.store)) {
		t.Fatal("refusal changed persisted state")
	}
	x.assertAbsent(t, request.MessageID)
	after, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("present malformed index was reset or spent quota", err)
	}
	after, err = os.ReadFile(canonicalPath)
	if err != nil || !bytes.Equal(after, canonical) {
		t.Fatal("refusal changed prior canonical record", err)
	}
}
func poisonExistingIndexField(fields map[string]json.RawMessage, kind string) {
	switch kind {
	case "missing-version":
		delete(fields, "version")
	case "missing-installation":
		delete(fields, "installation")
	case "missing-quota":
		delete(fields, "quota")
	case "empty-quota":
		fields["quota"] = json.RawMessage(`{}`)
	case "missing-entries":
		delete(fields, "entries")
	case "null-entries":
		fields["entries"] = json.RawMessage(`null`)
	}
}

func TestPluginInboxInstallationLoaderRejectsAbsentAuthority(t *testing.T) {
	for _, kind := range []string{"null", "empty-object", "missing-id", "missing-name", "missing-enabled", "missing-capabilities"} {
		t.Run(kind, func(t *testing.T) { checkInstallationAbsentAuthority(t, kind) })
	}
}
func checkInstallationAbsentAuthority(t *testing.T, kind string) {
	t.Helper()
	x := newInboxStorageFixture(t)
	r := x.request()
	if _, err := x.service.Send(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	path := x.f.pluginPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "null":
		raw = []byte(`null`)
	case "empty-object":
		raw = []byte(`{}`)
	default:
		delete(fields, kind[len("missing-"):])
		raw, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	next := x.request()
	m, err := x.service.Send(context.Background(), next)
	assertInboxFailure(t, m, err)
	x.assertAbsent(t, next.MessageID)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("installation loader invented authority", err)
	}
}

func TestPluginReadAuditLoaderRejectsNullAndMissingHeader(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"rows":[]}`, `{"version":0,"rows":[]}`} {
		t.Run(raw, func(t *testing.T) { checkReadAuditMissingHeader(t, []byte(raw)) })
	}
}
func checkReadAuditMissingHeader(t *testing.T, raw []byte) {
	t.Helper()
	x := newPluginReadFixture(t, "cursor")
	path := filepath.Join(x.f.store.Dir, "plugin-read-audit.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := x.f.ReadPluginMessage(x.target.ID, x.message.ID)
	if err == nil || len(result) != 0 {
		t.Fatal("owner audit loader invented a header")
	}
	result, code, err := x.f.ReadPluginMessageBound(&x.before, x.target.ID, x.message.ID)
	if err == nil || code != "audit_unavailable" || bytes.Contains(result, []byte(x.message.Body)) {
		t.Fatal("bound read audit loader borrowed defaults")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("malformed audit was reset", err)
	}
}

func TestPluginInboxFreshStateRequiresMissingFile(t *testing.T) {
	x := newInboxStorageFixture(t)
	path := filepath.Join(x.f.pluginDataPath(), "inbox.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("fixture not initially absent")
	}
	index, err := readFixtureInboxIndex(x, time.Now().UTC())
	if err != nil || index.Version != 1 || index.Installation != x.status.ID || !index.Quota.ValidHeader() {
		t.Fatal("missing file did not initialize fresh state", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read initialized a persistent file")
	}
	r := x.request()
	if _, err := x.service.Send(context.Background(), r); err != nil {
		t.Fatal("fresh admission failed", err)
	}
	checkPluginInboxEmptyIndexRoundTrip(t, x)
}

func checkPluginInboxEmptyIndexRoundTrip(t *testing.T, x inboxStorageFixture) {
	t.Helper()
	now := time.Now().UTC().Add(31 * 24 * time.Hour)
	index, err := readFixtureInboxIndex(x, now)
	if err != nil || index.Entries == nil || len(index.Entries) != 0 {
		t.Fatal("pruning must retain nonnil empty entries", err)
	}
	x.putIndex(t, index)
	reloaded, err := readFixtureInboxIndex(x, now)
	if err != nil || reloaded.Entries == nil || len(reloaded.Entries) != 0 {
		t.Fatal("persisted empty list must reload", err)
	}
}

func readFixtureInboxIndex(x inboxStorageFixture, now time.Time) (index pluginInboxIndex, err error) {
	err = WithJournalRead(x.f.store, func(view JournalView) error {
		var readErr error
		index, readErr = x.f.loadPluginInboxIndex(view, x.status.ID, now)
		return readErr
	})
	return index, err
}
