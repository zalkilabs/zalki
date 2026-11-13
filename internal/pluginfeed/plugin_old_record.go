package pluginfeed

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

// ErrOldPluginRecord marks a plugin record written by an older Zalki: one
// with the inbox_from and inbox_read grants (before receive), or an install
// receipt without the manifest id (before zalki-plugin/1). There is no
// conversion: the owner removes the plugin and installs it again.
var ErrOldPluginRecord = errors.New("plugin record from an older Zalki")

// oldPluginRecord reports the fix for a record with the old grant keys, or nil.
func oldPluginRecord(name string, raw []byte) error {
	source, old := oldPluginSource(raw)
	if !old {
		return nil
	}
	install := "zalki plugins install FOLDER"
	if source != "" && printableText(source, 1024) {
		install = "zalki plugins install " + shellWord(source)
	}
	return fmt.Errorf("%s: %w. Run: zalki plugins remove %s && %s", name, ErrOldPluginRecord, name, install)
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
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Install      *struct {
			Source string `json:"source"`
			ID     string `json:"id"`
		} `json:"install"`
	}
	if json.Unmarshal(raw, &old) != nil {
		return "", false
	}
	_, from := old.Capabilities["inbox_from"]
	_, inbox := old.Capabilities["inbox_read"]
	if old.Install == nil {
		return "", from || inbox
	}
	return old.Install.Source, from || inbox || old.Install.ID == ""
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
