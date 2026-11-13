package pluginfeed

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// maxPluginInboxBodyJSON leaves room for the envelope in a 64 KiB frame.
const maxPluginInboxBodyJSON = 60 << 10

// pluginInboxBodyFits reports whether a body still fits one frame after JSON
// escaping. Admission refuses a body that does not; inbox_next leaves such a
// body out of an older record admitted before this check.
func pluginInboxBodyFits(body string) bool {
	raw, err := json.Marshal(body)
	return err == nil && len(raw) <= maxPluginInboxBodyJSON
}

type pluginInboxEntry struct {
	ID         string    `json:"id"`
	Sender     string    `json:"sender"`
	Generation string    `json:"sender_generation"`
	Revision   string    `json:"admission_revision"`
	At         time.Time `json:"at"`
	Bytes      int       `json:"encoded_message_bytes"`
}

type pluginInboxIndex struct {
	Version      int                `json:"version"`
	Installation string             `json:"installation"`
	Entries      []pluginInboxEntry `json:"entries"`
	Quota        model.FloodState   `json:"quota"`
}

// Under Store flock: validate before pruning or admitting. Orphan reservations
// count just like readable entries until expiration; no body is stored here.
func (s pluginFiles) loadPluginInboxIndex(view bus.JournalView, installation string, now time.Time) (pluginInboxIndex, error) {
	var index pluginInboxIndex
	if err := pluginDirectory(s.pluginDataPath(), false); err != nil {
		return index, err
	}
	raw, err := journal.ReadBytes(filepath.Join(s.pluginDataPath(), "inbox.json"), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return pluginInboxIndex{Version: 1, Installation: installation, Quota: model.NewFloodState(now)}, nil
	}
	if err != nil {
		return index, err
	}
	if err = bus.DecodeStrictRecordJSON(raw, &index); err != nil {
		return index, err
	}
	if !validPluginInboxIndex(index, installation, now) {
		return index, journal.ErrInvalidFile
	}
	seen := make(map[string]bool, len(index.Entries))
	active := make([]pluginInboxEntry, 0, len(index.Entries))
	total := 0
	for _, entry := range index.Entries {
		if seen[entry.ID] || !validPluginInboxEntry(view, entry, now) {
			return index, journal.ErrInvalidFile
		}
		seen[entry.ID] = true
		if now.Sub(entry.At) >= 30*24*time.Hour {
			continue
		}
		total += entry.Bytes
		active = append(active, entry)
	}
	if total > 16<<20 {
		return index, journal.ErrInvalidFile
	}
	index.Entries = active
	return index, nil
}

// Reserve quota/index durably before the canonical record can be published.
// A matching orphan is an uncertain outcome, never a license to resend.
func (s pluginFiles) reservePluginInbox(index pluginInboxIndex, message model.Message, generation, revision string, now time.Time) error {
	total := 0
	for _, entry := range index.Entries {
		if entry.ID == message.ID {
			return model.NewError(model.CodeConflict, "an earlier send with this message id did not finish and its outcome is unknown; the id is used, so send again with a new message_id")
		}
		total += entry.Bytes
	}
	size, err := bus.EncodedMessageSize(message)
	if err != nil {
		return err
	}
	// The child gets the body inside one 64 KiB frame; escaping can grow it.
	if !pluginInboxBodyFits(message.Body) {
		return model.NewError(model.CodeInvalidInput, "message is too large for a plugin inbox after JSON escaping")
	}
	if len(index.Entries) >= 1000 || total+size > 16<<20 {
		return model.NewError(model.CodeRateLimited, "plugin inbox is full")
	}
	decision := model.DecideFloodAdmission(index.Quota, model.DeriveUUID("zalki/flood/sender/v1", message.From), message.ID, now)
	if decision.Code == model.CodeRateLimited {
		return model.FloodRetryError(decision.Reason, message.To, now, decision.RetryAt)
	}
	if decision.Code != "" {
		return model.NewError(decision.Code, "%s", decision.Reason)
	}
	if !decision.Changed {
		return model.NewError(model.CodeConflict, "an earlier send with this message id reserved quota and its outcome is unknown; the id is used, so send again with a new message_id")
	}
	index.Quota = decision.Next
	index.Entries = append(index.Entries, pluginInboxEntry{ID: message.ID, Sender: message.From, Generation: generation, Revision: revision, At: now, Bytes: size})
	return s.savePluginInboxIndex(index)
}

func validPluginInboxIndex(index pluginInboxIndex, installation string, now time.Time) bool {
	return index.Version == 1 && index.Installation == installation && index.Entries != nil && len(index.Entries) <= 1000 && index.Quota.ValidHeader() && !index.Quota.Inbound.At.After(now)
}

func validPluginInboxEntry(view bus.JournalView, entry pluginInboxEntry, now time.Time) bool {
	return view.CheckSessionKind(entry.Sender) == nil && model.IsCanonicalUUID(entry.ID) && model.IsCanonicalUUID(entry.Generation) && validPluginSHA(entry.Revision) && !entry.At.IsZero() && !entry.At.After(now) && entry.Bytes >= 1 && entry.Bytes <= bus.MaxMessageRecordBytes
}
