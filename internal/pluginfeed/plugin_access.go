package pluginfeed

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// ScopeAll is the only scope that gives standard access. Any other stored
// value, missing or unknown, means none.
const scopeAll = "all"

// PluginAccess is what the owner granted. Status and Receive are standard
// access: they apply to every live session in scope. Send and Read are
// extended access: exact, named sessions only, never widened by scope.
type PluginAccess struct {
	Status  bool `json:"status"`
	Receive bool `json:"receive"`
	// Send lists sessions the plugin may message unasked, as this plugin.
	Send []string `json:"send,omitempty"`
	// Read lists sessions whose received messages it may read.
	Read []string `json:"read,omitempty"`
}

func (a PluginAccess) empty() bool {
	return !a.Status && !a.Receive && len(a.Send)+len(a.Read) == 0
}

// messages reports access to message text or delivery: such a plugin's
// stderr is never shown.
func (a PluginAccess) messages() bool {
	return a.Receive || len(a.Send)+len(a.Read) > 0
}

func (a PluginAccess) clone() PluginAccess {
	a.Send, a.Read = slices.Clone(a.Send), slices.Clone(a.Read)
	return a
}

func (a *PluginAccess) sortLists() {
	for _, ids := range []*[]string{&a.Send, &a.Read} {
		*ids = slices.Clone(*ids)
		slices.Sort(*ids)
	}
}

func (s pluginFiles) checkAccess(a PluginAccess) error {
	for _, ids := range [][]string{a.Send, a.Read} {
		if err := s.checkPluginGrants(ids); err != nil {
			return err
		}
	}
	ids := slices.Concat(a.Send, a.Read)
	slices.Sort(ids)
	return s.checkPluginGrants(slices.Compact(ids))
}

// pluginRevision binds a launch to everything the owner decided, and to
// nothing about sessions coming and going: a session start, stop or retire
// never changes it, so a running child is never stopped by one. The
// settings line is fixed until plugin settings exist.
func pluginRevision(p pluginRecord) string {
	pin := ""
	if p.Launch != nil {
		pin = p.Launch.PinSHA256
	}
	lines := []string{
		"zalki-plugin-revision/1",
		"installation " + p.ID,
		"pin " + pin,
		"scope " + strconv.Quote(p.Scope),
		"status " + strconv.FormatBool(p.Access.Status),
		"receive " + strconv.FormatBool(p.Access.Receive),
		"send " + strings.Join(p.Access.Send, ","),
		"read " + strings.Join(p.Access.Read, ","),
		"settings {}",
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// standardMember is the use-time rule for standard access: scope all and a
// live session. Callers hold the journal view of the operation.
func standardMember(view bus.JournalView, p pluginRecord, session string) bool {
	if p.Scope != scopeAll {
		return false
	}
	member, err := view.PluginMember(session, p.Name)
	return err == nil && member
}

// extendedMember is the use-time rule for extended access: the exact named
// grant and a live session.
func extendedMember(view bus.JournalView, p pluginRecord, list []string, session string) bool {
	if !slices.Contains(list, session) {
		return false
	}
	member, err := view.PluginMember(session, p.Name)
	return err == nil && member
}

// ConfigurePluginScope sets the scope of standard access while disabled.
// Only "all" and "none" are accepted.
func (s PluginFeed) ConfigurePluginScope(scope string) error {
	if err := s.checkName(); err != nil {
		return err
	}
	if scope != scopeAll && scope != "none" {
		return model.NewError(model.CodeInvalidInput, "scope must be all or none")
	}
	return s.changeDisabled(func(p *pluginRecord) error {
		p.Scope = scope
		return nil
	})
}

// GrantPluginAccess replaces all granted access while disabled.
// Pin verification runs outside Store flock; the complete record is rechecked.
func (s PluginFeed) GrantPluginAccess(a PluginAccess) error {
	if err := s.checkName(); err != nil {
		return err
	}
	a.sortLists()
	if a.empty() {
		return model.NewError(model.CodeInvalidInput, "select some access or use plugins revoke")
	}
	if err := s.checkAccess(a); err != nil {
		return err
	}
	var before pluginRecord
	var prior [32]byte
	if err := bus.WithJournalRead(s.store, func(bus.JournalView) error {
		var raw []byte
		var err error
		before, raw, err = s.loadPlugin()
		prior = sha256.Sum256(raw)
		return err
	}); err != nil {
		return err
	}
	switch {
	case before.ID == "":
		return model.NewError(model.CodeNotFound, "add the plugin first")
	case before.Enabled:
		return model.NewError(model.CodeConflict, "disable the plugin before changing its access")
	case before.Launch == nil:
		return model.NewError(model.CodeInvalidInput, "access grants require an external plugin")
	}
	if err := before.Launch.Verify(); err != nil {
		return err
	}
	return s.locked(func() error {
		p, raw, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.ID != before.ID || p.Enabled || p.Launch == nil || sha256.Sum256(raw) != prior {
			return model.NewError(model.CodeConflict, "plugin changed before granting access; retry")
		}
		p.Access = a
		_, err = s.savePlugin(p)
		return err
	})
}

// RevokePluginAccess removes all access and atomically disables the host.
func (s PluginFeed) RevokePluginAccess() error {
	if err := s.checkName(); err != nil {
		return err
	}
	return s.locked(func() error {
		p, _, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.ID == "" {
			return model.NewError(model.CodeNotFound, "add the plugin first")
		}
		p.Access, p.Enabled = PluginAccess{}, false
		_, err = s.savePlugin(p)
		return err
	})
}

// changeDisabled applies one owner change to a disabled installation.
func (s PluginFeed) changeDisabled(change func(*pluginRecord) error) error {
	return s.locked(func() error {
		p, _, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.ID == "" {
			return model.NewError(model.CodeNotFound, "add the plugin first")
		}
		if p.Enabled {
			return model.NewError(model.CodeConflict, "disable the plugin before changing its access")
		}
		if err := change(&p); err != nil {
			return err
		}
		_, err = s.savePlugin(p)
		return err
	})
}
