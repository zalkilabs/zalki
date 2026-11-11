package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

type inboxReadFixture struct {
	inboxStorageFixture
	message model.Message
	now     time.Time
}

func newInboxReadFixture(t *testing.T, body string) inboxReadFixture {
	t.Helper()
	x := newInboxStorageFixture(t)
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.f.GrantPluginCapabilities(PluginCapabilityGrants{InboxRead: true, InboxFrom: []string{x.from.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	var err error
	x.status, err = x.f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if err = x.status.Launch.Verify(); err != nil {
		t.Fatal(err)
	}
	r := x.request()
	if body != "" {
		r.Body = body
	}
	m, err := x.service.Send(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return inboxReadFixture{x, m, time.Now().UTC()}
}
func (x inboxReadFixture) read(t *testing.T, before *PluginFeedStatus, now time.Time) ([]byte, error) {
	t.Helper()
	var raw []byte
	var readErr error
	err := x.f.write(func(view JournalView) error {
		raw, readErr = x.f.readPluginInboxAt(view, before, x.message.ID, now)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, readErr
}
func (x inboxReadFixture) audit(t *testing.T) pluginReadAudit {
	t.Helper()
	var log pluginReadAudit
	raw, err := os.ReadFile(filepath.Join(x.f.store.Dir, "plugin-inbox-read-audit.json"))
	if err != nil || json.Unmarshal(raw, &log) != nil || log.Version != 1 {
		t.Fatal("missing valid separate inbox audit", err)
	}
	if bytes.Contains(raw, []byte(x.message.Body)) || bytes.Contains(raw, []byte(x.key)) {
		t.Fatal("audit exposed body or credential")
	}
	return log
}
func assertInboxReadDenied(t *testing.T, raw []byte, err error, outcome string) {
	t.Helper()
	if err == nil || len(raw) != 0 || (outcome != "" && !strings.Contains(err.Error(), outcome)) {
		t.Fatal("inbox read released bytes or wrong refusal", len(raw), err, outcome)
	}
}
func (x inboxReadFixture) checkResult(t *testing.T, raw []byte, err error) pluginReadResult {
	t.Helper()
	var result pluginReadResult
	if err != nil || len(raw) > 32<<10 || json.Unmarshal(raw, &result) != nil || result.Code != "ok" || !canonicalEventID(result.Request) || result.Message == nil {
		t.Fatal("missing bounded owner result", len(raw), err)
	}
	m := result.Message
	entry := x.index(t).Entries[0]
	if m.ID != x.message.ID || m.To != x.message.To || m.From != x.from.ID || m.Body == nil || *m.Body != x.message.Body || m.SenderGeneration != x.from.Generation || m.AdmissionRevision != entry.Revision || bytes.Contains(raw, []byte(x.key)) {
		t.Fatal("owner result lost exact body/provenance or exposed key")
	}
	return result
}
func TestPluginInboxReadExactIsNonConsumingAndAudited(t *testing.T) {
	x := newInboxReadFixture(t, "")
	if err := os.WriteFile(filepath.Join(x.f.store.Dir, "messages", newID()+".json"), []byte("unrelated malformed private journal"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(x.f.store.Dir, "plugin-read-audit.json"), []byte(`{"version":1,"rows":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	before := journalSnapshot(t, x.f.store)
	raw, err := x.f.ReadPluginInboxMessage(x.message.ID)
	result := x.checkResult(t, raw, err)
	after := journalSnapshot(t, x.f.store)
	delete(after, "plugin-inbox-read-audit.json")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("owner read changed canonical/index/quota/native audit/installation state")
	}
	log := x.audit(t)
	if len(log.Rows) != 1 {
		t.Fatal("missing one audited release")
	}
	row := log.Rows[0]
	if row.Plugin != x.status.ID || row.Session != x.message.To || row.Message != x.message.ID || row.Request != result.Request || row.Revision != x.status.Capabilities.Revision || row.Outcome != "ok" || row.Bytes != len(raw) {
		t.Fatal("audit did not precede the matching response")
	}
	// A fresh owner proxy must respect persistent rate state, without ACK or replay.
	raw, err = NewPluginFeed(x.f.store, x.f.name).ReadPluginInboxMessage(x.message.ID)
	assertInboxReadDenied(t, raw, err, "rate_limited")
}
func TestPluginInboxReadBodyAndEncodedResponseBounds(t *testing.T) {
	for _, c := range []struct {
		name, body string
		ok         bool
	}{{"full-16KiB", strings.Repeat("a", 16<<10), true}, {"escaped-over-32KiB", strings.Repeat("<", 16<<10), false}} {
		t.Run(c.name, func(t *testing.T) {
			x := newInboxReadFixture(t, c.body)
			raw, err := x.f.ReadPluginInboxMessage(x.message.ID)
			if c.ok {
				x.checkResult(t, raw, err)
			} else {
				assertInboxReadDenied(t, raw, err, "too_large")
				log := x.audit(t)
				if len(log.Rows) != 1 || log.Rows[0].Outcome != "too_large" || log.Rows[0].Bytes != 0 {
					t.Fatal("oversize result was not audited as a zero-byte refusal")
				}
			}
		})
	}
}
