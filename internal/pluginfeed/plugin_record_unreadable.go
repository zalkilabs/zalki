package pluginfeed

import (
	"encoding/json"
	"errors"

	"github.com/zalkilabs/zalki/internal/model"
)

// ErrPluginRecordUnreadable marks a plugin record whose content cannot be read
// or does not pass the record checks: it is refused, never used. The one fix is
// to remove the plugin.
var ErrPluginRecordUnreadable = errors.New("plugin record cannot be read")

// The reasons a record is unreadable, one per content check on the read path.
// They are fixed names: the command log carries one, never an error text.
const (
	reasonDecode   = "decode"
	reasonIdentity = "identity"
	reasonAccess   = "access"
	reasonRevision = "revision"
	reasonEpoch    = "epoch"
)

// unreadablePluginRecordError is the error text for any such record. It names no
// path and no cause; the cause stays reachable with errors.Is and errors.As.
type unreadablePluginRecordError struct {
	name   string
	reason string
	cause  error
}

func (e unreadablePluginRecordError) Error() string {
	return "plugin record " + e.name + " cannot be read; remove it with: zalki plugins remove " + e.name
}

func (e unreadablePluginRecordError) Unwrap() error { return e.cause }

func (e unreadablePluginRecordError) Is(target error) bool {
	return target == ErrPluginRecordUnreadable
}

// LogReason is the fixed reason name for the command log.
func (e unreadablePluginRecordError) LogReason() string { return e.reason }

// unreadableRecord is what a load returns for a record that cannot be read: only
// the installation ID of the record (when it is a canonical UUID), so that remove
// can find the files of that installation, and the one plain error.
func unreadableRecord(name, reason string, raw []byte, cause error) (pluginRecord, []byte, error) {
	var record struct {
		ID string `json:"id"`
	}
	id := ""
	if json.Unmarshal(raw, &record) == nil && model.IsCanonicalUUID(record.ID) {
		id = record.ID
	}
	return pluginRecord{ID: id}, nil, unreadablePluginRecordError{name, reason, cause}
}
