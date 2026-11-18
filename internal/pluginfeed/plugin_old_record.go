package pluginfeed

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

// ErrOldPluginRecord marks a plugin record written by an older Zalki: one
// without a scope (before rule scope, so also with the inbox_from/inbox_read
// grants of before receive), or an install receipt without the manifest id
// (before zalki-plugin/1), or a receipt without its installer and declared
// access (before install provenance), or one with the retired owner-record
// cursor/gap fields. There is no conversion: keep its record and data,
// and install a separate instance.
var ErrOldPluginRecord = errors.New("plugin record from an older Zalki")

// oldPluginRecord reports the fix for a record with the old grant keys, or nil.
func oldPluginRecord(name string, raw []byte) error {
	source, old := oldPluginSource(raw)
	if !old {
		return nil
	}
	// Only a bounded absolute path is printed; a relative or flag-looking
	// source would mean something else when pasted, so it becomes FOLDER.
	install := "zalki plugins install FOLDER"
	if filepath.IsAbs(source) && printableText(source, 1024) {
		install = "zalki plugins install " + shellWord(source)
	}
	// A separate name leaves the old record and data intact if install refuses.
	return fmt.Errorf("%s: %w. Keep the old record and its data. To install a separate instance, choose an unused name and run: %s --as NEW_NAME", name, ErrOldPluginRecord, install)
}

// shellWord quotes a path for a POSIX shell when it has any character outside
// a small safe set, so the printed command works when pasted.
func shellWord(value string) string {
	if value != "" && !strings.ContainsFunc(value, shellUnsafe) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// shellUnsafe: any character outside letters, digits and /._-+@:,=.
func shellUnsafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("/._-+@:,=", r)
}

// oldPluginSource reports whether raw is an old record, and the source
// folder of its install receipt when it has one.
func oldPluginSource(raw []byte) (string, bool) {
	var old struct {
		Scope        *string                    `json:"scope"`
		Enabled      json.RawMessage            `json:"enabled"`
		Epoch        *string                    `json:"epoch"`
		Position     json.RawMessage            `json:"position"`
		GapID        json.RawMessage            `json:"gap_id"`
		GapReason    json.RawMessage            `json:"gap_reason"`
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Install      *struct {
			Source      string          `json:"source"`
			ID          string          `json:"id"`
			InstalledBy *string         `json:"installed_by"`
			Access      json.RawMessage `json:"access"`
		} `json:"install"`
	}
	if json.Unmarshal(raw, &old) != nil {
		return "", false
	}
	_, from := old.Capabilities["inbox_from"]
	_, inbox := old.Capabilities["inbox_read"]
	noScope := old.Scope == nil
	oldFeed := hasOldPluginFeedFields(old.Position, old.GapID, old.GapReason, strings.TrimSpace(string(old.Enabled)) == "true", old.Epoch)
	if old.Install == nil {
		return "", from || inbox || noScope || oldFeed
	}
	// A receipt from before install provenance is old: it is never completed
	// with a guessed installer.
	noProvenance := old.Install.InstalledBy == nil || old.Install.Access == nil
	return old.Install.Source, from || inbox || noScope || oldFeed || old.Install.ID == "" || noProvenance
}

func hasOldPluginFeedFields(position, gapID, gapReason json.RawMessage, enabled bool, epoch *string) bool {
	return position != nil || gapID != nil || gapReason != nil || (enabled && epoch == nil)
}

// oldPluginID reads only the installation ID of an old record, for remove.
func oldPluginID(raw []byte) string {
	var old struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &old) != nil || !model.IsCanonicalUUID(old.ID) {
		return ""
	}
	return old.ID
}
