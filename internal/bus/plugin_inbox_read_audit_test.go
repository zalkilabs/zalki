package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxReadAuditFailureReturnsNoBody(t *testing.T) {
	for _, c := range []struct{ name, raw string }{{"null", `null`}, {"object", `{}`}, {"missing-version", `{"rows":[]}`}, {"missing-rows", `{"version":1}`}, {"null-rows", `{"version":1,"rows":null}`}, {"unknown", `{"version":1,"rows":[],"body":"PRIVATE"}`}, {"oversize", strings.Repeat("x", (1<<20)+1)}} {
		t.Run(c.name, func(t *testing.T) { checkInboxReadAuditRaw(t, []byte(c.raw)) })
	}
	t.Run("symlink", checkInboxReadAuditSymlink)
	for _, mode := range []os.FileMode{0500, 0300} {
		t.Run(mode.String(), func(t *testing.T) { checkInboxReadAuditWriteFailure(t, mode) })
	}
}
func checkInboxReadAuditRaw(t *testing.T, raw []byte) {
	t.Helper()
	x := newInboxReadFixture(t, "")
	path := filepath.Join(x.f.store.Dir, "plugin-inbox-read-audit.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := x.f.ReadPluginInboxMessage(x.message.ID)
	assertInboxReadDenied(t, out, err, "audit_unavailable")
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("malformed existing audit was reset", err)
	}
}
func checkInboxReadAuditSymlink(t *testing.T) {
	t.Helper()
	x := newInboxReadFixture(t, "")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside trap"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(x.f.store.Dir, "plugin-inbox-read-audit.json")); err != nil {
		t.Fatal(err)
	}
	out, err := x.f.ReadPluginInboxMessage(x.message.ID)
	assertInboxReadDenied(t, out, err, "audit_unavailable")
	raw, err := os.ReadFile(outside)
	if err != nil || string(raw) != "outside trap" {
		t.Fatal("audit followed outside symlink", err)
	}
}
func checkInboxReadAuditWriteFailure(t *testing.T, mode os.FileMode) {
	t.Helper()
	x := newInboxReadFixture(t, "")
	if os.Geteuid() == 0 {
		t.Fatal("permission fault requires ordinary user")
	}
	var raw []byte
	var readErr error
	if err := x.f.locked(func() error {
		if err := os.Chmod(x.f.store.Dir, mode); err != nil {
			return err
		}
		defer func() {
			if err := os.Chmod(x.f.store.Dir, 0700); err != nil {
				t.Error(err)
			}
		}()
		raw, readErr = x.f.readPluginInboxAt(&x.status, x.message.ID, x.now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertInboxReadDenied(t, raw, readErr, "audit_unavailable")
	if mode == 0300 {
		log := x.audit(t)
		if len(log.Rows) != 1 || log.Rows[0].Outcome != "ok" {
			t.Fatal("fault was not after audit rename")
		}
	}
}
func TestPluginInboxReadAuditRoundTripRetentionAndPersistentRate(t *testing.T) {
	x := newInboxReadFixture(t, "")
	raw, err := x.read(t, &x.status, x.now)
	x.checkResult(t, raw, err)
	log := x.audit(t)
	// Genuine writer state must round-trip, including an explicit empty array.
	rows, err := loadInboxReadAuditForTest(x.f, x.now)
	if err != nil || len(rows) != 1 || rows[0].Request != log.Rows[0].Request {
		t.Fatal("genuine audit did not reload", err)
	}
	for _, now := range []time.Time{x.now.Add(100 * time.Millisecond), x.now.Add(-time.Hour)} {
		out, err := x.read(t, &x.status, now)
		assertInboxReadDenied(t, out, err, "rate_limited")
	}
	fresh := NewPluginFeed(x.f.store, x.f.name)
	rows, err = loadInboxReadAuditForTest(fresh, x.now)
	if err != nil || len(rows) != 1 {
		t.Fatal("fresh proxy lost persistent state", err)
	}
	raw, err = x.read(t, &x.status, x.now.Add(time.Second))
	x.checkResult(t, raw, err)
	if err = fresh.savePluginReadAuditFile("plugin-inbox-read-audit.json", []pluginReadAuditRow{}, x.now); err != nil {
		t.Fatal(err)
	}
	rows, err = loadInboxReadAuditForTest(fresh, x.now)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("valid empty audit did not round-trip", err)
	}
	checkInboxReadAuditLimits(t, x, log.Rows[0])
}
func loadInboxReadAuditForTest(f PluginFeed, now time.Time) ([]pluginReadAuditRow, error) {
	return f.loadPluginReadAuditFile("plugin-inbox-read-audit.json", now, func(row pluginReadAuditRow) error {
		if row.Session != "plugin:"+row.Plugin {
			return journal.ErrInvalidFile
		}
		return nil
	}, true)
}
func checkInboxReadAuditLimits(t *testing.T, x inboxReadFixture, base pluginReadAuditRow) {
	t.Helper()
	base.At = x.now.Add(-time.Hour)
	rows := make([]pluginReadAuditRow, 1001)
	for i := range rows {
		rows[i] = base
		rows[i].Request = newID()
	}
	if err := x.f.savePluginReadAuditFile("plugin-inbox-read-audit.json", rows, x.now); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadInboxReadAuditForTest(x.f, x.now)
	if err != nil || len(loaded) != 1000 {
		t.Fatal("inbox audit exceeded row limit", err)
	}
	rows = []pluginReadAuditRow{base, base}
	rows[0].At = x.now.Add(-31 * 24 * time.Hour)
	rows[1].At = x.now.Add(-30 * 24 * time.Hour)
	if err = x.f.savePluginReadAuditFile("plugin-inbox-read-audit.json", rows, x.now); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadInboxReadAuditForTest(x.f, x.now)
	if err != nil || len(loaded) != 1 {
		t.Fatal("audit retention boundary changed", err)
	}
	// An invalid existing row must fail, rather than be trimmed into fresh state.
	rows[1].Session = x.from.ID
	raw, err := json.Marshal(pluginReadAudit{Version: 1, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(x.f.store.Dir, "plugin-inbox-read-audit.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := x.read(t, &x.status, x.now)
	assertInboxReadDenied(t, out, err, "audit_unavailable")
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("audit corruption was repaired", err)
	}
}
func TestPluginInboxReadConcurrentRequestsReleaseOnce(t *testing.T) {
	x := newInboxReadFixture(t, "")
	type reply struct {
		raw []byte
		err error
	}
	done := make(chan reply, 4)
	for range 4 {
		go func() {
			var r reply
			err := x.f.locked(func() error { r.raw, r.err = x.f.readPluginInboxAt(&x.status, x.message.ID, x.now); return nil })
			if err != nil {
				r.err = err
			}
			done <- r
		}()
	}
	released, limited := 0, 0
	for range 4 {
		r := <-done
		switch {
		case r.err == nil && len(r.raw) > 0:
			released++
		case model.ErrorCodeOf(r.err) == model.CodeRateLimited && len(r.raw) == 0:
			limited++
		default:
			t.Fatal("unexpected concurrent result", len(r.raw), r.err)
		}
	}
	if released != 1 || limited != 3 || len(x.audit(t).Rows) != 1 {
		t.Fatal("concurrent requests released more than one body", released, limited)
	}
}
