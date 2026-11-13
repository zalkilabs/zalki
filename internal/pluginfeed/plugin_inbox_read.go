package pluginfeed

import (
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// ReadPluginInboxMessage is an exact-ID nonconsuming owner proxy. It uses no
// native actor key and does not prove that an external process is running.
func (s PluginFeed) ReadPluginInboxMessage(id string) ([]byte, error) {
	if !model.IsCanonicalUUID(id) || s.checkName() != nil {
		return nil, pluginReadFailure("invalid_input")
	}
	before, err := s.PluginStatus()
	if err != nil {
		return nil, pluginReadFailure("unavailable")
	}
	if before.ID == "" || before.Launch == nil || !before.Access.Receive {
		return nil, pluginReadFailure("denied")
	}
	if err = before.Launch.Verify(); err != nil {
		return nil, pluginReadFailure("stale_binding")
	}
	var raw []byte
	err = s.write(func(view bus.JournalView) error {
		var err error
		raw, err = s.readPluginInboxAt(view, &before, id, time.Now().UTC())
		return err
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (s pluginFiles) readPluginInboxAt(view bus.JournalView, before *PluginFeedStatus, id string, now time.Time) ([]byte, error) {
	p, _, err := s.loadPlugin()
	if err != nil {
		return nil, pluginReadFailure("unavailable")
	}
	rows, err := s.loadPluginReadAuditFile("plugin-inbox-read-audit.json", now, func(row pluginReadAuditRow) error {
		if row.Session != "plugin:"+row.Plugin {
			return journal.ErrInvalidFile
		}
		return nil
	})
	if err != nil {
		return nil, pluginReadFailure("audit_unavailable")
	}
	result := pluginReadResult{Request: model.NewUUID(), Code: pluginInboxReadAuthorization(p, before)}
	if result.Code == "ok" {
		for _, row := range rows {
			if row.Plugin == before.ID && now.Sub(row.At) < time.Second {
				return nil, pluginReadFailure("rate_limited")
			}
		}
		result.Message, result.Code = s.ownPluginInboxMessage(view, before.ID, id, now)
	}
	save := func(rows []pluginReadAuditRow, now time.Time) error {
		return s.savePluginReadAuditFile("plugin-inbox-read-audit.json", rows, now)
	}
	raw, _, err := finishPluginReadResult(before, "plugin:"+before.ID, id, result, rows, now, save)
	return raw, err
}

func pluginInboxReadAuthorization(p pluginRecord, before *PluginFeedStatus) string {
	if code := launchBindingCode(p, before); code != "ok" {
		return code
	}
	if !p.Access.Receive {
		return "denied"
	}
	return "ok"
}

// launchBindingCode compares the record with the launch snapshot of a running
// child: "ok" while the child may still act, "revoked" when the plugin is
// disabled, else "stale_binding". Every enable gives a new epoch, and access
// changes only while disabled, so a child never adopts newer access. The
// revision covers every owner decision and no session membership, so a
// session starting or stopping never stops a child.
func launchBindingCode(p pluginRecord, launch *PluginFeedStatus) string {
	switch {
	case launch == nil || p.ID != launch.ID || p.Launch == nil:
		return "stale_binding"
	case !p.Enabled:
		return "revoked"
	case p.Epoch != launch.Epoch || p.Revision != launch.Revision || !samePin(p.Launch, launch.Launch):
		return "stale_binding"
	default:
		return "ok"
	}
}

func samePin(a, b *PluginLaunch) bool {
	return a == nil && b == nil || a != nil && b != nil && a.PinSHA256 == b.PinSHA256
}

// PluginBindingCode reports whether a running child with this launch snapshot
// may still run: "ok", "revoked" or "stale_binding".
func (s PluginFeed) PluginBindingCode(launch *PluginFeedStatus) string {
	code := "stale_binding"
	_ = bus.WithJournalRead(s.store, func(bus.JournalView) error { // a failed read keeps "stale_binding"
		p, _, err := s.loadPlugin()
		if err == nil {
			code = launchBindingCode(p, launch)
		}
		return err
	})
	return code
}

func (s pluginFiles) ownPluginInboxMessage(view bus.JournalView, installation, id string, now time.Time) (*pluginReadMessage, string) {
	index, err := s.loadPluginInboxIndex(view, installation, now)
	if err != nil {
		return nil, "unavailable"
	}
	var found *pluginInboxEntry
	for i := range index.Entries {
		if index.Entries[i].ID == id {
			found = &index.Entries[i]
			break
		}
	}
	if found == nil {
		return nil, "denied"
	}
	p, _, err := s.loadPlugin()
	if err != nil {
		return nil, "unavailable"
	}
	if inboxEntryState(view, p, *found) != "ok" {
		return nil, "unavailable"
	}
	m, size, err := view.CurrentMessage(id)
	if err != nil || !pluginInboxRecordMatches(&m, installation, *found, size) {
		return nil, "denied"
	}
	if model.ValidateMessageBody(m.Body, "") != nil {
		return nil, "too_large"
	}
	return &pluginReadMessage{ID: m.ID, From: m.From, To: m.To, Body: &m.Body, SenderGeneration: found.Generation, AdmissionRevision: found.Revision}, "ok"
}

func pluginInboxRecordMatches(m *model.Message, installation string, entry pluginInboxEntry, encodedBytes int) bool {
	return m.Kind == "" && m.To == "plugin:"+installation && m.TargetGeneration == installation && m.From == entry.Sender && m.CreatedAt == entry.At.Format(time.RFC3339Nano) && encodedBytes == entry.Bytes && !m.Held && !m.BodyOmitted && !m.BodyDeferred && m.ReplyTo == "" && m.Status == model.StatusSubmitted && m.AcknowledgedAt == "" && validPluginInboxReceipt(m)
}

func validPluginInboxReceipt(m *model.Message) bool {
	return m.Receipt != nil && m.Receipt.Status == model.StatusSubmitted && m.Receipt.At == m.CreatedAt && m.Receipt.Code == "" && m.Receipt.Error == "" && m.Receipt.Output == ""
}
