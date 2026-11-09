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

type pluginReadFixture struct {
	f          PluginFeed
	executable string
	target     model.Session
	message    model.Message
	before     PluginFeedStatus
	now        time.Time
}

func newPluginReadFixture(t *testing.T, kind string) pluginReadFixture {
	t.Helper()
	f, executable, sender := externalPinFixture(t)
	target, _ := register(t, f.store, kind)
	m := model.Message{ID: newID(), From: sender, To: target.ID, TargetGeneration: target.Generation, Body: "private body <>& secret", CreatedAt: "2000-01-01T00:00:00Z", Status: model.StatusSubmitted}
	if err := f.store.saveMessage(m); err != nil {
		t.Fatal(err)
	}
	if err := f.GrantPluginCapabilities(PluginCapabilityGrants{Read: []string{target.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	before, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	return pluginReadFixture{f, executable, target, m, before, time.Now().UTC()}
}

func (x pluginReadFixture) read(t *testing.T, before *PluginFeedStatus, session, id string, now time.Time) ([]byte, error) {
	t.Helper()
	var raw []byte
	var readErr error
	if err := x.f.locked(func() error { raw, readErr = x.f.readPluginMessageAt(before, session, id, now); return nil }); err != nil {
		t.Fatal(err)
	}
	return raw, readErr
}

func (x pluginReadFixture) assertDenied(t *testing.T, before *PluginFeedStatus, outcome string) {
	t.Helper()
	raw, err := x.read(t, before, x.target.ID, x.message.ID, x.now)
	if len(raw) != 0 || err == nil || !strings.Contains(err.Error(), "plugin read "+outcome) || strings.Contains(err.Error(), x.message.Body) {
		t.Fatal("authorization fence failed", outcome, len(raw), err)
	}
}

func (x pluginReadFixture) audit(t *testing.T, now time.Time) []pluginReadAuditRow {
	t.Helper()
	rows, err := x.f.loadPluginReadAudit(now)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPluginReadExactOldInboxIsNonConsumingAndAudited(t *testing.T) {
	x := newPluginReadFixture(t, "cursor")
	offer, err := x.f.NextPluginFeed()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	if err := filepath.WalkDir(x.f.store.Dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		files[path] = raw
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	// Exact lookup must not scan unrelated malformed journal records.
	if err := os.WriteFile(filepath.Join(x.f.store.Dir, "messages", newID()+".json"), []byte("bad unrelated JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := x.f.ReadPluginMessage(x.target.ID, x.message.ID)
	result := assertPluginReadExactResult(t, raw, err, &x)
	for path, before := range files {
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatal("read consumed or changed canonical state", path, readErr)
		}
	}
	assertPluginReadSuccessAudit(t, &x, &result, len(raw))
	auditRaw, err := os.ReadFile(filepath.Join(x.f.store.Dir, "plugin-read-audit.json"))
	if err != nil || bytes.Contains(auditRaw, []byte("private body")) || bytes.Contains(auditRaw, []byte("secret")) {
		t.Fatal("audit leaked body", err)
	}
	if offer.ID != "" {
		if err := x.f.AckPluginFeed(offer); err != nil {
			t.Fatal("audit invalidated metadata receipt", err)
		}
	}
}

func TestPluginReadSeparateAuthorityAndFreshBinding(t *testing.T) {
	cases := []struct {
		name, outcome string
		edit          func(*pluginRecord)
	}{
		{"disabled", "revoked", func(p *pluginRecord) { p.Enabled = false }},
		{"installation", "stale_binding", func(p *pluginRecord) { p.ID = newID(); p.Capabilities.Installation = p.ID }},
		{"revision", "stale_binding", func(p *pluginRecord) { p.Capabilities.Revision = newID() }},
		{"capability-pin", "stale_binding", func(p *pluginRecord) { p.Capabilities.PinSHA256 = strings.Repeat("a", 64) }},
		{"launch-pin", "stale_binding", func(p *pluginRecord) { p.Launch.PinSHA256 = strings.Repeat("b", 64) }},
		{"paired-pin", "stale_binding", func(p *pluginRecord) {
			p.Launch.PinSHA256 = strings.Repeat("c", 64)
			p.Capabilities.PinSHA256 = p.Launch.PinSHA256
		}},
		{"no-capabilities", "stale_binding", func(p *pluginRecord) { p.Capabilities = nil }},
		{"send-only", "denied", func(p *pluginRecord) { p.Capabilities.Send = p.Capabilities.Read; p.Capabilities.Read = nil }},
		{"inbox-read-only", "denied", func(p *pluginRecord) { p.Capabilities.Read = nil; p.Capabilities.InboxRead = true }},
		{"inbox-from-only", "denied", func(p *pluginRecord) { p.Capabilities.InboxFrom = p.Capabilities.Read; p.Capabilities.Read = nil }},
		{"other-read", "denied", func(p *pluginRecord) { p.Capabilities.Read = []string{p.Sessions[0]} }},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			x := newPluginReadFixture(t, "cursor")
			p, _, err := x.f.loadPlugin()
			if err != nil {
				t.Fatal(err)
			}
			row.edit(&p)
			if _, err := x.f.savePlugin(p); err != nil {
				t.Fatal(err)
			}
			x.assertDenied(t, &x.before, row.outcome)
			rows := x.audit(t, x.now)
			if len(rows) != 1 || rows[0].Outcome != row.outcome || rows[0].Bytes != 0 {
				t.Fatal("denial audit wrong", rows)
			}
		})
	}
	for _, action := range []string{"revoke", "regrant", "readd"} {
		t.Run(action, func(t *testing.T) { pluginReadTransitionFence(t, action) })
	}

	t.Run("public-pin-drift", func(t *testing.T) {
		x := newPluginReadFixture(t, "cursor")
		if err := os.WriteFile(x.executable, []byte("#!/bin/sh\nexit 9\n"), 0700); err != nil {
			t.Fatal(err)
		}
		raw, err := x.f.ReadPluginMessage(x.target.ID, x.message.ID)
		if len(raw) != 0 || model.ErrorCodeOf(err) != model.CodeConflict {
			t.Fatal("drift exposed body", len(raw), err)
		}
	})
}

func TestPluginReadRecipientGenerationAndRecordFences(t *testing.T) {
	cases := []struct {
		name, outcome string
		edit          func(*model.Message)
	}{
		{"sent-only", "denied", func(m *model.Message) { m.From, m.To = m.To, m.From }},
		{"stale-generation", "stale_target", func(m *model.Message) { m.TargetGeneration = newID() }},
		{"held", "unavailable", func(m *model.Message) { m.Held = true }},
		{"not-sent", "unavailable", func(m *model.Message) { m.Status = model.StatusNotSent }},
		{"unknown-status", "unavailable", func(m *model.Message) { m.Status = "unknown" }},
		{"plugin-kind", "denied", func(m *model.Message) { m.Kind = "plugin" }},
		{"to-granted-session-invalid-from-accepted-unavailable", "unavailable", func(m *model.Message) { m.From = "private body secret" }},
		{"to-granted-session-malformed-generation-accepted-stale-target", "stale_target", func(m *model.Message) { m.TargetGeneration = "not-a-generation" }},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			x := newPluginReadFixture(t, "cursor")
			row.edit(&x.message)
			if err := x.f.store.saveMessage(x.message); err != nil {
				t.Fatal(err)
			}
			x.assertDenied(t, &x.before, row.outcome)
		})
	}
	for _, contents := range []string{"{private body secret", `{"schema":99}`, `{"schema":1,"body":"private body secret","unknown":true}`} {
		t.Run("malformed", func(t *testing.T) {
			x := newPluginReadFixture(t, "cursor")
			if err := os.WriteFile(filepath.Join(x.f.store.Dir, "messages", x.message.ID+".json"), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			x.assertDenied(t, &x.before, "denied")
		})
	}
	t.Run("missing", func(t *testing.T) {
		x := newPluginReadFixture(t, "cursor")
		if err := os.Remove(filepath.Join(x.f.store.Dir, "messages", x.message.ID+".json")); err != nil {
			t.Fatal(err)
		}
		x.assertDenied(t, &x.before, "denied")
	})
	t.Run("absent-foreign-malformed-denials-match", pluginReadDenialEquality)

}

func TestPluginReadNativeDeferredThenAcknowledged(t *testing.T) {
	x := newPluginReadFixture(t, "codex")
	raw, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now)
	var result pluginReadResult
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Code != "native_deferred" || result.Message == nil || result.Message.Body != nil || bytes.Contains(raw, []byte("private body")) {
		t.Fatal("native body escaped before acknowledgement", len(raw), err)
	}
	x.message.AcknowledgedAt = x.now.Format(time.RFC3339Nano)
	if err := x.f.store.saveMessage(x.message); err != nil {
		t.Fatal(err)
	}
	raw, err = x.read(t, &x.before, x.target.ID, x.message.ID, x.now.Add(2*time.Second))
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Code != "ok" || result.Message.Body == nil || *result.Message.Body != x.message.Body {
		t.Fatal("acknowledged native body unavailable", len(raw), err)
	}
}

func TestPluginReadRawAndEncodedBoundsWithoutTruncation(t *testing.T) {
	for _, row := range []struct {
		name, body string
		ok         bool
	}{{"empty", "", true}, {"raw-limit", strings.Repeat("x", 16<<10), true}, {"raw-over", strings.Repeat("x", (16<<10)+1), false}, {"encoded-over", strings.Repeat("\x01", 6000), false}} {
		t.Run(row.name, func(t *testing.T) {
			x := newPluginReadFixture(t, "cursor")
			x.message.Body = row.body
			if err := x.f.store.saveMessage(x.message); err != nil {
				t.Fatal(err)
			}
			raw, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now)
			if !row.ok {
				if len(raw) != 0 || err == nil || !strings.Contains(err.Error(), "too_large") {
					t.Fatal("oversized result escaped", len(raw), err)
				}
				return
			}
			var result pluginReadResult
			if err != nil || len(raw) > 32<<10 || json.Unmarshal(raw, &result) != nil || result.Message == nil || result.Message.Body == nil || *result.Message.Body != row.body {
				t.Fatal("body was truncated or empty body lost", len(raw), err)
			}
		})
	}
}

func TestPluginReadPersistentRateAndClockRollback(t *testing.T) {
	x := newPluginReadFixture(t, "cursor")
	if _, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{-time.Hour, 0, 999 * time.Millisecond} {
		raw, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now.Add(offset))
		if len(raw) != 0 || model.ErrorCodeOf(err) != model.CodeRateLimited {
			t.Fatal("persistent rate fence failed", offset, len(raw), err)
		}
	}
	if rows := x.audit(t, x.now); len(rows) != 1 {
		t.Fatal("rate limited retry grew audit", len(rows))
	}
	if _, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now.Add(time.Second)); err != nil {
		t.Fatal("rate window did not reopen", err)
	}
	if err := x.f.RevokePluginCapabilities(); err != nil {
		t.Fatal(err)
	}
	raw, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now.Add(time.Second))
	if len(raw) != 0 || err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatal("rate hid terminal revoke", len(raw), err)
	}
}

func TestPluginReadRejectsInvalidSelectorsAndDefaultAuthority(t *testing.T) {
	x := newPluginReadFixture(t, "cursor")
	for _, pair := range [][2]string{{"display-name", x.message.ID}, {"plugin:" + newID(), x.message.ID}, {x.target.ID, "../message"}, {x.target.ID, "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"}, {strings.Repeat("s", 4096), x.message.ID}} {
		raw, err := x.f.ReadPluginMessage(pair[0], pair[1])
		if len(raw) != 0 || model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("invalid selector accepted", len(raw), err)
		}
	}
	f, _, session := externalPinFixture(t)
	raw, err := f.ReadPluginMessage(session, newID())
	if len(raw) != 0 || model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("metadata grant implied read authority", len(raw), err)
	}
	// Public preflight and under-lock auth both remain fail closed.
	if err := x.f.RevokePluginCapabilities(); err != nil {
		t.Fatal(err)
	}
	raw, err = x.f.ReadPluginMessage(x.target.ID, x.message.ID)
	if len(raw) != 0 || err == nil {
		t.Fatal("public revoke allowed body", len(raw), err)
	}
}

func TestPluginReadAuditFailureDeniesBeforeRelease(t *testing.T) {
	for _, row := range []struct{ name, raw string }{{"malformed", "private body secret"}, {"version", `{"version":2,"rows":[]}`}, {"unknown", `{"version":1,"rows":[],"private":"secret"}`}, {"oversize", strings.Repeat("x", (1<<20)+1)}} {
		t.Run(row.name, func(t *testing.T) {
			x := newPluginReadFixture(t, "cursor")
			if err := os.WriteFile(filepath.Join(x.f.store.Dir, "plugin-read-audit.json"), []byte(row.raw), 0600); err != nil {
				t.Fatal(err)
			}
			x.assertDenied(t, &x.before, "audit_unavailable")
		})
	}
	t.Run("symlink", func(t *testing.T) {
		x := newPluginReadFixture(t, "cursor")
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(x.f.store.Dir, "plugin-read-audit.json")); err != nil {
			t.Fatal(err)
		}
		x.assertDenied(t, &x.before, "audit_unavailable")
		raw, _ := os.ReadFile(outside)
		if string(raw) != "{}" {
			t.Fatal("audit followed symlink")
		}
	})
	t.Run("post-rename-directory-sync", pluginReadAuditSyncFailure)
}

func TestPluginReadAuditRetentionAndValidation(t *testing.T) {
	x := newPluginReadFixture(t, "cursor")
	base := pluginReadAuditRow{At: x.now.Add(-time.Hour), Plugin: x.before.ID, Request: newID(), Session: x.target.ID, Message: x.message.ID, Revision: x.before.Capabilities.Revision, Outcome: "ok", Bytes: 200}
	rows := make([]pluginReadAuditRow, 1001)
	for i := range rows {
		rows[i] = base
		rows[i].Request = newID()
	}
	if err := x.f.savePluginReadAudit(rows, x.now); err != nil {
		t.Fatal(err)
	}
	got := x.audit(t, x.now)
	if len(got) != 1000 || got[0].Request != rows[1].Request {
		t.Fatal("audit row cap failed", len(got))
	}
	rows = []pluginReadAuditRow{base, base}
	rows[0].At = x.now.Add(-31 * 24 * time.Hour)
	rows[1].At = x.now.Add(-30 * 24 * time.Hour)
	if err := x.f.savePluginReadAudit(rows, x.now); err != nil {
		t.Fatal(err)
	}
	if got := x.audit(t, x.now); len(got) != 1 || !got[0].At.Equal(rows[1].At) {
		t.Fatal("audit age limit failed", len(got))
	}
	// Admission may not evict the one-second rate window to fit a full file.
	for i := range rows {
		rows[i].At = x.now
	}
	full := make([]pluginReadAuditRow, 1001)
	for i := range full {
		full[i] = rows[0]
	}
	if err := x.f.savePluginReadAudit(full, x.now); err == nil {
		t.Fatal("audit evicted active rate window")
	}
	assertPluginReadInvalidAuditRows(t, &x, &base)
	// Reader must reject a valid-but-over-count file, rather than trim corruption.
	raw, err := journal.EncodeJSON(pluginReadAudit{Version: 1, Rows: full}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(x.f.store.Dir, "plugin-read-audit.json"), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.loadPluginReadAudit(x.now); err == nil {
		t.Fatal("over-count audit accepted")
	}
}

func pluginReadAuditSyncFailure(t *testing.T) {
	t.Helper()

	x := newPluginReadFixture(t, "cursor")
	if os.Geteuid() == 0 {
		t.Skip("directory sync permission cut requires ordinary user")
	}
	var raw []byte
	var readErr error
	if err := x.f.locked(func() error {
		if err := os.Chmod(x.f.store.Dir, 0300); err != nil {
			return err
		}
		defer func() {
			if err := os.Chmod(x.f.store.Dir, 0700); err != nil {
				t.Error(err)
			}
		}()
		raw, readErr = x.f.readPluginMessageAt(&x.before, x.target.ID, x.message.ID, x.now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 || readErr == nil || !strings.Contains(readErr.Error(), "audit_unavailable") {
		t.Fatal("audit failure released body", len(raw), readErr)
	}
	rows := x.audit(t, x.now)
	if len(rows) != 1 || rows[0].Outcome != "ok" {
		t.Fatal("failure was not after audit rename", rows)
	}

}

func pluginReadTransitionFence(t *testing.T, action string) {
	t.Helper()

	x := newPluginReadFixture(t, "cursor")
	if err := x.f.RevokePluginCapabilities(); err != nil {
		t.Fatal(err)
	}
	outcome := "revoked"
	if action == "readd" {
		if err := x.f.RemovePlugin(); err != nil {
			t.Fatal(err)
		}
		if err := x.f.AddExternalPlugin(x.executable, []string{"$HOME", "literal space"}); err != nil {
			t.Fatal(err)
		}
		if err := x.f.ConfigurePluginFeed([]string{x.target.ID}); err != nil {
			t.Fatal(err)
		}
		outcome = "stale_binding"
	}
	if action != "revoke" {
		if err := x.f.GrantPluginCapabilities(PluginCapabilityGrants{Read: []string{x.target.ID}}); err != nil {
			t.Fatal(err)
		}
		if err := x.f.EnablePluginFeed(true); err != nil {
			t.Fatal(err)
		}
		outcome = "stale_binding"
	}
	x.assertDenied(t, &x.before, outcome)
}

func assertPluginReadExactResult(t *testing.T, raw []byte, err error, x *pluginReadFixture) pluginReadResult {
	t.Helper()
	var result pluginReadResult
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Code != "ok" || result.Message == nil || result.Message.Body == nil || *result.Message.Body != x.message.Body || !canonicalEventID(result.Request) {
		t.Fatal("exact old inbox read failed", len(raw), err)
	}
	return result
}

func assertPluginReadSuccessAudit(t *testing.T, x *pluginReadFixture, result *pluginReadResult, size int) {
	t.Helper()
	rows := x.audit(t, time.Now().UTC())
	if len(rows) != 1 || rows[0].Request != result.Request || rows[0].Plugin != x.before.ID || rows[0].Revision != x.before.Capabilities.Revision || rows[0].Session != x.target.ID || rows[0].Message != x.message.ID || rows[0].Outcome != "ok" || rows[0].Bytes != size {
		t.Fatal("durable audit missing or wrong", rows)
	}
}

func assertPluginReadInvalidAuditRows(t *testing.T, x *pluginReadFixture, base *pluginReadAuditRow) {
	t.Helper()

	for _, edit := range []func(*pluginReadAuditRow){func(r *pluginReadAuditRow) { r.Bytes = -1 }, func(r *pluginReadAuditRow) { r.Outcome = "private body secret" }, func(r *pluginReadAuditRow) { r.Plugin = "bad" }, func(r *pluginReadAuditRow) { r.Session = "name" }, func(r *pluginReadAuditRow) { r.At = time.Time{} }} {
		bad := *base
		edit(&bad)
		raw, err := journal.EncodeJSON(pluginReadAudit{Version: 1, Rows: []pluginReadAuditRow{bad}}, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.WriteAtomic(filepath.Join(x.f.store.Dir, "plugin-read-audit.json"), raw); err != nil {
			t.Fatal(err)
		}
		if _, err := x.f.loadPluginReadAudit(x.now); err == nil {
			t.Fatal("invalid audit row accepted")
		}
	}
}

func TestPluginReadConcurrentRequestsHaveOneAuditedRelease(t *testing.T) {
	x := newPluginReadFixture(t, "cursor")
	type response struct {
		raw []byte
		err error
	}
	done := make(chan response, 4)
	for range 4 {
		go func() {
			var r response
			lockErr := x.f.locked(func() error {
				r.raw, r.err = x.f.readPluginMessageAt(&x.before, x.target.ID, x.message.ID, x.now)
				return nil
			})
			if lockErr != nil {
				r.err = lockErr
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
			t.Fatal("concurrent read fence failed", len(r.raw), r.err)
		}
	}
	if released != 1 || limited != 3 || len(x.audit(t, x.now)) != 1 {
		t.Fatal("concurrent reads released more than one body", released, limited)
	}
}

func pluginReadDenialEquality(t *testing.T) {
	t.Helper()
	x := newPluginReadFixture(t, "cursor")
	cases := []struct {
		name string
		raw  []byte
	}{
		{"absent", nil},
		{"foreign", pluginReadForeignRecord(t, &x, false)},
		{"malformed-json", []byte("{private body secret")},
		{"foreign-with-malformed-sender-and-generation", pluginReadForeignRecord(t, &x, true)},
		{"wrong-schema", []byte(`{"schema":99}`)},
	}
	if err := os.Remove(filepath.Join(x.f.store.Dir, "messages", x.message.ID+".json")); err != nil {
		t.Fatal(err)
	}
	firstError, first := pluginReadFixedDenial(t, &x, x.now)
	for i, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			path := filepath.Join(x.f.store.Dir, "messages", x.message.ID+".json")
			if row.raw == nil {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, row.raw, 0600); err != nil {
				t.Fatal(err)
			}
			text, audit := pluginReadFixedDenial(t, &x, x.now.Add(time.Duration(i+1)*2*time.Second))
			// Request UUID/time are fresh; all semantic audit fields must match for
			// the same requested message/session/installation, including outcome/bytes.
			audit.Request, audit.At = first.Request, first.At
			if text != firstError || audit != first {
				t.Fatal("read exposed foreign message existence", text, audit)
			}
		})
	}
}

func pluginReadForeignRecord(t *testing.T, x *pluginReadFixture, malformed bool) []byte {
	t.Helper()
	message := x.message
	message.Schema = recordSchema
	message.To = message.From
	if malformed {
		message.From = "malformed sender secret"
		message.TargetGeneration = "not-a-generation"
	}
	raw, err := journal.EncodeJSON(message, maxStateFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func pluginReadFixedDenial(t *testing.T, x *pluginReadFixture, now time.Time) (string, pluginReadAuditRow) {
	t.Helper()
	raw, err := x.read(t, &x.before, x.target.ID, x.message.ID, now)
	if len(raw) != 0 || err == nil || model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("read denial shape changed", len(raw), err)
	}
	rows := x.audit(t, now)
	if len(rows) == 0 {
		t.Fatal("denial lacked audit")
	}
	row := rows[len(rows)-1]
	if row.Outcome != "denied" || row.Bytes != 0 {
		t.Fatal("denial audit shape changed", row)
	}
	return err.Error(), row
}
