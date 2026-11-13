package pluginfeed

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// Installed-by values that are not a session: an anonymous install the owner
// reviewed and confirmed in a real terminal, or an installer Zalki could not
// prove (--yes, or credentials that failed).
const (
	installedByTerminal = "terminal"
	installedByUnproven = "unproven"
)

// declaredAccess is the access the reviewed manifest asked for: four
// booleans. It is what the author declared, not what was granted; the
// current record holds the grants and may narrow later.
type declaredAccess struct {
	Status  bool `json:"status"`
	Receive bool `json:"receive"`
	Send    bool `json:"send"`
	Read    bool `json:"read"`
}

// decodeDeclaredAccess accepts only the object with exactly the four keys,
// in the order status, receive, send, read, each a JSON boolean. Whitespace
// does not matter (the record file is indented); anything else does.
func decodeDeclaredAccess(raw json.RawMessage) (declaredAccess, bool) {
	var d declaredAccess
	var fields map[string]json.RawMessage
	var compact bytes.Buffer
	if len(raw) == 0 || len(raw) > 512 || json.Compact(&compact, raw) != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 4 {
		return d, false
	}
	for _, key := range []string{"status", "receive", "send", "read"} {
		value, ok := fields[key]
		if !ok || (string(value) != "true" && string(value) != "false") {
			return d, false
		}
	}
	if json.Unmarshal(raw, &d) != nil {
		return d, false
	}
	canonical, err := json.Marshal(d)
	return d, err == nil && string(canonical) == compact.String()
}

func (d declaredAccess) extended() bool { return d.Send || d.Read }

// covers reports whether every granted kind was declared.
func (d declaredAccess) covers(a PluginAccess) bool {
	return (!a.Status || d.Status) && (!a.Receive || d.Receive) && (len(a.Send) == 0 || d.Send) && (len(a.Read) == 0 || d.Read)
}

// validProvenance checks the stored installer: a native session (with a safe
// historical label), terminal, or unproven (both without a label).
func validProvenance(by, label string) bool {
	switch by {
	case installedByTerminal, installedByUnproven:
		return label == ""
	}
	return model.ValidateSessionAddress(by) == nil && !model.IsPluginInboxAddress(by) && label != "" && safeLabel(label) == label
}

// safeLabel keeps a display label only when it is printable ASCII of at most
// 64 bytes; otherwise it is "". It is a historical claim, never a lookup key.
func safeLabel(label string) string {
	if label == "" || len(label) > 64 || strings.ContainsFunc(label, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
		return ""
	}
	return label
}

// installProvenance decides the installer under the commit lock. Credentials
// are checked now, after the build: a valid native session is the installer,
// labelled by its current name or its ID. An anonymous reviewed terminal is
// "terminal". Anything else, including credentials that no longer prove
// (rotated, retired, exited, wrong), is "unproven", never terminal. The
// capability is used only here and is never stored.
func installProvenance(view bus.JournalView, actor, capability string, terminalReview bool) (by, label string) {
	if actor == "" && capability == "" {
		if terminalReview {
			return installedByTerminal, ""
		}
		return installedByUnproven, ""
	}
	name, err := view.AuthenticatedSessionLabel(actor, capability)
	if err != nil {
		return installedByUnproven, ""
	}
	if label = safeLabel(name); label == "" {
		label = actor
	}
	return actor, label
}
