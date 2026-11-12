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

// The reply rule: a plugin that may receive from a session may answer it, but
// only about a message that session sent it, for a short time, a few times.
const (
	replyWindow    = 24 * time.Hour
	maxReplies     = 8
	replyLedgerTTL = replyWindow + time.Hour
)

// replyClock is the reply rule's clock; tests move it.
var replyClock = time.Now

// Reply refusals. Each one maps to its own result code for the plugin.
var (
	errNotAReply    = model.NewError(model.CodeUnauthorized, "plugin reply does not answer a message sent to this plugin")
	errReplyExpired = model.NewError(model.CodeUnauthorized, "plugin reply is too late: the message is older than 24 hours")
	errReplyLimit   = model.NewError(model.CodeRateLimited, "plugin reply limit reached: 8 replies per message")
	errOutOfScope   = model.NewError(model.CodeUnauthorized, "plugin reply target is not in the plugin's receive scope now")
)

// PluginSendCode names a refused plugin send for the plugin, or "" when err is
// not a reply refusal.
func PluginSendCode(err error) string {
	switch {
	case errors.Is(err, errNotAReply):
		return "not_a_reply"
	case errors.Is(err, errReplyExpired):
		return "reply_expired"
	case errors.Is(err, errReplyLimit):
		return "reply_limit"
	case errors.Is(err, errOutOfScope):
		return "out_of_scope"
	}
	return ""
}

// replyLedger counts the stored replies per parent message. It lives next to
// the plugin record (plugins/NAME.replies.json), outside the plugin's own data
// folder, and only the host writes it, under Store flock. This guards against
// honest bugs and relayed text; a native plugin running as the same user is
// not stopped by it.
type replyLedger struct {
	Version      int                    `json:"version"`
	Installation string                 `json:"installation"`
	Parents      map[string]replyParent `json:"parents"`
}

type replyParent struct {
	At      time.Time `json:"at"` // the parent's creation time
	Replies []string  `json:"replies"`
}

func (s pluginFiles) replyLedgerPath() string {
	return filepath.Join(s.pluginDir(), s.name+".replies.json")
}

// loadReplyLedger reads the ledger and drops parents older than the TTL.
func (s pluginFiles) loadReplyLedger(installation string, now time.Time) (replyLedger, error) {
	ledger := replyLedger{Version: 1, Installation: installation, Parents: map[string]replyParent{}}
	raw, err := journal.ReadBytes(s.replyLedgerPath(), 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		return ledger, err
	}
	var read replyLedger
	if err := bus.DecodeStrictRecordJSON(raw, &read); err != nil {
		return ledger, err
	}
	if read.Version != 1 || !model.IsCanonicalUUID(read.Installation) || read.Parents == nil {
		return ledger, journal.ErrInvalidFile
	}
	if read.Installation != installation {
		return ledger, nil // left by an earlier installation of this name
	}
	for parent, entry := range read.Parents {
		if !model.IsCanonicalUUID(parent) || len(entry.Replies) > maxReplies || !allUUIDs(entry.Replies) {
			return ledger, journal.ErrInvalidFile
		}
		if now.Sub(entry.At) < replyLedgerTTL {
			ledger.Parents[parent] = entry
		}
	}
	return ledger, nil
}

func allUUIDs(ids []string) bool {
	for _, id := range ids {
		if !model.IsCanonicalUUID(id) {
			return false
		}
	}
	return true
}

// checkReply applies the reply rule under Store flock. It returns the record
// step that counts the new reply once it is stored.
func (s pluginFiles) checkReply(view bus.JournalView, p pluginRecord, to, parentID, messageID string, now time.Time) (func() error, error) {
	created, err := replyParentTime(view, p, to, parentID)
	if err != nil {
		return nil, err
	}
	ledger, err := s.loadReplyLedger(p.ID, now)
	if err != nil {
		return nil, err
	}
	entry := ledger.Parents[parentID]
	counted := slices.Contains(entry.Replies, messageID)
	if counted && replyStored(view, messageID) {
		// A retry of a stored reply returns that message, even after the
		// window: nothing new is sent and nothing is counted again.
		return func() error { return nil }, nil
	}
	// A new reply, or one that was counted but never stored (it failed after
	// its count), must pass the rules that hold now.
	if now.Sub(created) >= replyWindow {
		return nil, errReplyExpired // a retry of a refused reply is refused again; no quota is used
	}
	if !slices.Contains(p.Capabilities.Receive, to) {
		return nil, errOutOfScope
	}
	if counted {
		return func() error { return nil }, nil // its count is already spent
	}
	if len(entry.Replies) >= maxReplies {
		return nil, errReplyLimit
	}
	return func() error {
		entry.At = created
		entry.Replies = append(entry.Replies, messageID)
		ledger.Parents[parentID] = entry
		return s.saveReplyLedger(ledger)
	}, nil
}

// replyParentTime checks that parentID was sent to THIS installation by a
// native session, and that the reply goes back to that session. It returns
// the parent's creation time.
func replyParentTime(view bus.JournalView, p pluginRecord, to, parentID string) (time.Time, error) {
	parent, _, err := view.CurrentMessage(parentID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return time.Time{}, errNotAReply
	case err != nil:
		return time.Time{}, err
	}
	if parent.Kind != "" || parent.To != "plugin:"+p.ID || parent.TargetGeneration != p.ID || model.IsPluginInboxAddress(parent.From) || model.ValidateSessionAddress(parent.From) != nil || parent.From != to {
		return time.Time{}, errNotAReply
	}
	created, err := time.Parse(time.RFC3339Nano, parent.CreatedAt)
	if err != nil {
		return time.Time{}, errNotAReply
	}
	return created, nil
}

// replyStored reports whether the reply message record exists.
func replyStored(view bus.JournalView, messageID string) bool {
	_, _, err := view.CurrentMessage(messageID)
	return err == nil
}

func (s pluginFiles) saveReplyLedger(ledger replyLedger) error {
	raw, err := journal.EncodeJSON(ledger, 1<<20)
	if err != nil {
		return err
	}
	return journal.WriteAtomic(s.replyLedgerPath(), raw)
}
