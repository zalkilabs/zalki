package pluginfeed

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type auditRowsFixture struct {
	f       PluginFeed
	before  PluginFeedStatus
	message model.Message
	session string
	now     time.Time
	native  bool
}

func newAuditRowsFixture(t *testing.T, namespace string) auditRowsFixture {
	t.Helper()
	if namespace == "native" {
		x := newPluginReadFixture(t, "cursor")
		return auditRowsFixture{x.f, x.before, x.message, x.target.ID, x.now, true}
	}
	x := newInboxReadFixture(t, "")
	return auditRowsFixture{x.f, x.status, x.message, x.message.To, x.now, false}
}
func (x auditRowsFixture) path() string {
	name := "plugin-inbox-read-audit.json"
	if x.native {
		name = "plugin-read-audit.json"
	}
	return filepath.Join(x.f.store.Dir, name)
}
func (x auditRowsFixture) owner() ([]byte, error) {
	if x.native {
		return NewPluginFeed(x.f.store, x.f.name).ReadPluginMessage(x.session, x.message.ID)
	}
	return NewPluginFeed(x.f.store, x.f.name).ReadPluginInboxMessage(x.message.ID)
}
func (x auditRowsFixture) load() ([]pluginReadAuditRow, error) {
	if x.native {
		return x.f.loadPluginReadAudit(x.now)
	}
	return loadInboxReadAuditForTest(x.f, x.now)
}
func (x auditRowsFixture) prime(t *testing.T) pluginReadAudit {
	t.Helper()
	raw, err := x.owner()
	var result pluginReadResult
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Code != "ok" || result.Message == nil || result.Message.Body == nil || *result.Message.Body != x.message.Body {
		t.Fatal("genuine initial audited read failed", err)
	}
	raw, err = os.ReadFile(x.path())
	var log pluginReadAudit
	if err != nil || json.Unmarshal(raw, &log) != nil || log.Version != 1 || len(log.Rows) != 1 {
		t.Fatal("genuine read did not persist one audit row", err)
	}
	return log
}
func assertAuditRowsUnchanged(t *testing.T, x auditRowsFixture, before map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, journalSnapshot(t, x.f.store)) {
		t.Fatal("malformed audit denial changed audit/canonical/native state")
	}
}
func assertAuditRowsFailure(t *testing.T, raw []byte, err error) {
	t.Helper()
	if len(raw) != 0 || err == nil || !strings.Contains(err.Error(), "audit_unavailable") {
		t.Fatal("malformed audit released message bytes or wrong error", len(raw), err)
	}
}

func TestNativeReadAuditMissingOrNullRowsNeverResetState(t *testing.T) {
	for _, kind := range []string{"missing", "null"} {
		for _, api := range []string{"owner", "bound"} {
			t.Run(kind+"/"+api, func(t *testing.T) { checkNativeAuditMissingRows(t, kind, api) })
		}
	}
}
func checkNativeAuditMissingRows(t *testing.T, kind, api string) {
	t.Helper()
	x := newAuditRowsFixture(t, "native")
	x.prime(t)
	poison := []byte(`{"version":1}`)
	if kind == "null" {
		poison = []byte(`{"version":1,"rows":null}`)
	}
	if err := os.WriteFile(x.path(), poison, 0600); err != nil {
		t.Fatal(err)
	}
	before := journalSnapshot(t, x.f.store)
	if api == "owner" {
		raw, err := x.owner()
		assertAuditRowsFailure(t, raw, err)
	} else {
		checkNativeAuditBoundFailure(t, x)
	}
	assertAuditRowsUnchanged(t, x, before)
}
func checkNativeAuditBoundFailure(t *testing.T, x auditRowsFixture) {
	t.Helper()
	if err := x.before.Launch.Verify(); err != nil {
		t.Fatal(err)
	}
	raw, code, err := x.f.ReadPluginMessageBound(&x.before, x.session, x.message.ID)
	var result pluginReadResult
	if err == nil || code != "audit_unavailable" || json.Unmarshal(raw, &result) != nil || result.Code != code || !model.IsCanonicalUUID(result.Request) || result.Message != nil {
		t.Fatal("bound malformed audit released message data or lost fixed error/request", code, err)
	}
}
func TestReadAuditWriterEmptyAndPopulatedStateRoundTrips(t *testing.T) {
	for _, namespace := range []string{"native", "inbox"} {
		for _, kind := range []string{"nil", "empty", "populated"} {
			t.Run(namespace+"/"+kind, func(t *testing.T) { checkAuditRowsRoundTrip(t, namespace, kind) })
		}
	}
}
func checkAuditRowsRoundTrip(t *testing.T, namespace, kind string) {
	t.Helper()
	x := newAuditRowsFixture(t, namespace)
	var rows []pluginReadAuditRow
	switch kind {
	case "empty":
		rows = []pluginReadAuditRow{}
	case "populated":
		rows = []pluginReadAuditRow{{At: x.now.Add(-time.Hour), Plugin: x.before.ID, Request: model.NewUUID(), Session: x.session, Message: x.message.ID, Revision: x.before.Capabilities.Revision, Outcome: "ok", Bytes: 200}}
	}
	if err := x.f.savePluginReadAuditFile(filepath.Base(x.path()), rows, x.now); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(x.path())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if kind != "populated" && !bytes.Equal(bytes.TrimSpace(fields["rows"]), []byte(`[]`)) {
		t.Fatal("empty writer emitted absent/null rows instead of an array")
	}
	got, err := x.load()
	if err != nil || got == nil || len(got) != len(rows) {
		t.Fatal("valid written audit did not round-trip", err)
	}
	if kind == "populated" && !reflect.DeepEqual(got, rows) {
		t.Fatal("writer changed a genuine populated row")
	}
	after, err := os.ReadFile(x.path())
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("loader rewrote valid audit bytes", err)
	}
}
func TestReadAuditMissingFileIsLazyAndFirstReadIsAudited(t *testing.T) {
	for _, namespace := range []string{"native", "inbox"} {
		t.Run(namespace, func(t *testing.T) {
			x := newAuditRowsFixture(t, namespace)
			if _, err := os.Stat(x.path()); !os.IsNotExist(err) {
				t.Fatal("fixture audit was not absent", err)
			}
			before := journalSnapshot(t, x.f.store)
			rows, err := x.load()
			if err != nil || len(rows) != 0 {
				t.Fatal("absent audit did not load empty", err)
			}
			assertAuditRowsUnchanged(t, x, before)
			x.prime(t)
		})
	}
}
func TestReadAuditRecipientValidatorsRemainSeparate(t *testing.T) {
	for _, namespace := range []string{"native", "inbox"} {
		t.Run(namespace, func(t *testing.T) {
			x := newAuditRowsFixture(t, namespace)
			log := x.prime(t)
			log.Rows[0].At = x.now.Add(-time.Hour)
			if x.native {
				log.Rows[0].Session = "plugin:" + x.before.ID
			} else {
				log.Rows[0].Session = "cursor:" + model.NewUUID()
			}
			x.poisonRow(t, log)
			before := journalSnapshot(t, x.f.store)
			raw, err := x.owner()
			assertAuditRowsFailure(t, raw, err)
			assertAuditRowsUnchanged(t, x, before)
		})
	}
}
func (x auditRowsFixture) poisonRow(t *testing.T, log pluginReadAudit) {
	t.Helper()
	raw, err := journal.EncodeJSON(log, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(x.path(), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestNativeReadAuditRowPredicatesFailBeforeRelease(t *testing.T) {
	for _, kind := range []string{"at", "plugin", "request", "message", "revision", "negative-bytes", "oversize-bytes", "outcome"} {
		t.Run(kind, func(t *testing.T) {
			x := newAuditRowsFixture(t, "native")
			log := x.prime(t)
			log.Rows[0].At = x.now.Add(-time.Hour)
			editAuditRowsPredicate(&log.Rows[0], kind)
			x.poisonRow(t, log)
			before := journalSnapshot(t, x.f.store)
			raw, err := x.owner()
			assertAuditRowsFailure(t, raw, err)
			assertAuditRowsUnchanged(t, x, before)
		})
	}
}
func editAuditRowsPredicate(row *pluginReadAuditRow, kind string) {
	switch kind {
	case "at":
		row.At = time.Time{}
	case "plugin":
		row.Plugin = "invalid"
	case "request":
		row.Request = "invalid"
	case "message":
		row.Message = "invalid"
	case "revision":
		row.Revision = "invalid"
	case "negative-bytes":
		row.Bytes = -1
	case "oversize-bytes":
		row.Bytes = (32 << 10) + 1
	case "outcome":
		row.Outcome = "invalid"
	}
}

func TestReadAuditReadErrorsNeverInitializeOrRewrite(t *testing.T) {
	for _, namespace := range []string{"native", "inbox"} {
		for _, kind := range []string{"oversize", "symlink"} {
			t.Run(namespace+"/"+kind, func(t *testing.T) { checkAuditRowsReadError(t, namespace, kind) })
		}
	}
}
func checkAuditRowsReadError(t *testing.T, namespace, kind string) {
	t.Helper()
	x := newAuditRowsFixture(t, namespace)
	outside := filepath.Join(t.TempDir(), "outside")
	outsideBytes := []byte(`{"version":1,"rows":[]}`)
	if kind == "symlink" {
		if err := os.WriteFile(outside, outsideBytes, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, x.path()); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(x.path(), bytes.Repeat([]byte("x"), (1<<20)+1), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := journalSnapshot(t, x.f.store)
	if rows, err := x.load(); err == nil || len(rows) != 0 {
		t.Fatal("read failure was treated as fresh missing audit", err)
	}
	assertAuditRowsUnchanged(t, x, before)
	if kind == "symlink" {
		raw, err := os.ReadFile(outside)
		if err != nil || !bytes.Equal(raw, outsideBytes) {
			t.Fatal("audit followed/changed outside target", err)
		}
	}
}
