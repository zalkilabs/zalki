package pluginfeed

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// PluginInboxMessage is one message of a plugin's own inbox as the child sees
// it. Every field comes from the core record and index, never from the body.
type PluginInboxMessage struct {
	ID               string `json:"id"`
	From             string `json:"from"`
	SenderGeneration string `json:"sender_generation"`
	To               string `json:"to"`
	CreatedAt        string `json:"created_at"`
	Body             string `json:"body,omitempty"`
	// BodyBytes is set, and Body left out, when the body is too large for one
	// frame (code too_large). Only records admitted before the cap can be.
	BodyBytes int `json:"body_bytes,omitempty"`
}

// NextPluginInbox returns the oldest unconsumed message, or nil when the inbox
// is empty. The same message comes back until the child consumes it. A body
// too large for one frame is left out, with code "too_large".
func (s PluginFeed) NextPluginInbox(before *PluginFeedStatus) (*PluginInboxMessage, string) {
	var result *PluginInboxMessage
	code := "unauthorized"
	err := s.write(func(view bus.JournalView) error {
		if code = s.pluginInboxCode(before); code != "ok" {
			return nil
		}
		result, code = s.nextPluginInboxAt(view, before.ID, time.Now().UTC())
		return nil
	})
	if err != nil {
		return nil, "unavailable"
	}
	return result, code
}

// ConsumePluginInbox durably acknowledges one message of the plugin's own
// inbox, then drops its index entry. Consuming it again returns "ok".
func (s PluginFeed) ConsumePluginInbox(before *PluginFeedStatus, id string) string {
	if !model.IsCanonicalUUID(id) {
		return "invalid_input"
	}
	code := "unauthorized"
	err := s.write(func(view bus.JournalView) error {
		if code = s.pluginInboxCode(before); code != "ok" {
			return nil
		}
		code = s.consumePluginInboxAt(view, before.ID, id, time.Now().UTC())
		return nil
	})
	if err != nil {
		return "unavailable"
	}
	return code
}

// BoundPluginSend returns the send check for the child launched with binding.
// It runs under the journal lock before any session lookup or write: the
// record must still match the binding (installation, enabled, pin, grant
// revision). A reply (replyTo set) must pass the reply rule; any other send
// needs the extended send grant for the exact recipient. The returned record
// step counts a new reply in the same lock.
func BoundPluginSend(binding *PluginFeedStatus) func(view bus.JournalView, stateDir, installation, to, replyTo, messageID string) (func() error, error) {
	return func(view bus.JournalView, stateDir, installation, to, replyTo, messageID string) (func() error, error) {
		var record func() error
		err := view.WithWriteScope(stateDir, func() error {
			if binding == nil || binding.ID != installation || binding.Launch == nil {
				return model.NewError(model.CodeUnauthorized, "plugin send denied")
			}
			f, p, err := pluginInboxTarget(stateDir, "plugin:"+installation)
			if err != nil || launchBindingCode(p, binding) != "ok" {
				return model.NewError(model.CodeUnauthorized, "plugin send denied")
			}
			if replyTo == "" {
				if !extendedMember(view, p, p.Access.Send, to) {
					return model.NewError(model.CodeUnauthorized, "plugin send denied")
				}
				record = func() error { return nil }
				return nil
			}
			count, err := f.checkReply(view, p, to, replyTo, messageID, replyClock())
			if err != nil {
				return err
			}
			record = func() error { return view.WithWriteScope(stateDir, count) }
			return nil
		})
		return record, err
	}
}

// pluginInboxCode checks the bound installation under the Store lock.
func (s pluginFiles) pluginInboxCode(before *PluginFeedStatus) string {
	if before == nil || before.ID == "" || before.Launch == nil || !before.Access.Receive {
		return "unauthorized"
	}
	p, _, err := s.loadPlugin()
	if err != nil {
		return "unavailable"
	}
	if code := pluginInboxReadAuthorization(p, before); code != "denied" {
		return code
	}
	return "unauthorized"
}

// Caller holds Store flock. Entries whose record is already acknowledged were
// consumed before a crash removed them; they are dropped here (repair).
func (s pluginFiles) nextPluginInboxAt(view bus.JournalView, installation string, now time.Time) (*PluginInboxMessage, string) {
	index, err := s.loadPluginInboxIndex(view, installation, now)
	if err != nil {
		return nil, "unavailable"
	}
	p, _, err := s.loadPlugin()
	if err != nil {
		return nil, "unavailable"
	}
	var result *PluginInboxMessage
	kept := make([]pluginInboxEntry, 0, len(index.Entries))
	for _, entry := range index.Entries {
		m, size, err := view.CurrentMessage(entry.ID)
		if err == nil && consumedPluginInboxRecord(&m, installation, entry) {
			continue
		}
		state := inboxEntryState(view, p, entry)
		if state == "dead" {
			continue // only the index entry goes; the message record stays stored
		}
		kept = append(kept, entry)
		if result == nil && state == "ok" && err == nil && pluginInboxRecordMatches(&m, installation, entry, size) {
			result = &PluginInboxMessage{ID: m.ID, From: m.From, SenderGeneration: entry.Generation, To: m.To, CreatedAt: m.CreatedAt, Body: m.Body}
		}
	}
	if len(kept) != len(index.Entries) {
		index.Entries = kept
		if s.savePluginInboxIndex(index) != nil {
			return nil, "unavailable"
		}
	}
	if result != nil && !pluginInboxBodyFits(result.Body) {
		result.BodyBytes, result.Body = len(result.Body), ""
		return result, "too_large"
	}
	return result, "ok"
}

// inboxEntryState decides whether an inbox entry may be offered now, by the
// sender's identity (actor ID), not its registration generation. "ok": the
// plugin has receive access and the sender is a member now (scope all, live).
// "held": it is not; the entry stays and comes back if the sender is a member
// again. "dead": the sender's session
// record is gone, so the entry can never be offered again. The stored
// generation is information for the plugin only.
func inboxEntryState(view bus.JournalView, p pluginRecord, entry pluginInboxEntry) string {
	_, err := view.SessionGeneration(entry.Sender)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "dead"
	case err != nil || !p.Access.Receive || !standardMember(view, p, entry.Sender):
		return "held"
	default:
		return "ok"
	}
}

// Caller holds Store flock. The record ACK comes first: a crash before the
// index write leaves an acknowledged entry, which nextPluginInboxAt drops.
func (s pluginFiles) consumePluginInboxAt(view bus.JournalView, installation, id string, now time.Time) string {
	index, err := s.loadPluginInboxIndex(view, installation, now)
	if err != nil {
		return "unavailable"
	}
	i := slices.IndexFunc(index.Entries, func(e pluginInboxEntry) bool { return e.ID == id })
	if i < 0 {
		m, _, err := view.CurrentMessage(id)
		if err == nil && m.Kind == "" && m.To == "plugin:"+installation && m.AcknowledgedAt != "" {
			return "ok"
		}
		return "not_found"
	}
	entry := index.Entries[i]
	m, size, err := view.CurrentMessage(id)
	if err != nil || !pluginInboxRecordMatches(&m, installation, entry, size) && !consumedPluginInboxRecord(&m, installation, entry) {
		return "not_found"
	}
	if err := view.AckPluginInboxMessage(s.root, installation, id); err != nil {
		return "unavailable"
	}
	index.Entries = slices.Delete(index.Entries, i, i+1)
	if s.savePluginInboxIndex(index) != nil {
		return "unavailable"
	}
	return "ok"
}

func consumedPluginInboxRecord(m *model.Message, installation string, entry pluginInboxEntry) bool {
	return m.Kind == "" && m.To == "plugin:"+installation && m.From == entry.Sender && m.AcknowledgedAt != ""
}

func (s pluginFiles) savePluginInboxIndex(index pluginInboxIndex) error {
	encoded, err := journal.EncodeJSON(index, 1<<20)
	if err != nil {
		return err
	}
	return journal.WriteAtomic(filepath.Join(s.pluginDataPath(), "inbox.json"), encoded)
}
