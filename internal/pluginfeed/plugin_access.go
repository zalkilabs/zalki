package pluginfeed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
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

// ConfigurePluginScope sets the scope of standard access. Only "all" and
// "none" are accepted. On an enabled plugin it rebinds (see changeOwnerAccess).
func (s PluginFeed) ConfigurePluginScope(scope string) error {
	if err := s.checkName(); err != nil {
		return err
	}
	if scope != scopeAll && scope != "none" {
		return model.NewError(model.CodeInvalidInput, "scope must be all or none")
	}
	return s.changeOwnerAccess(func(p *pluginRecord) error {
		p.Scope = scope
		return nil
	})
}

// GrantPluginAccess replaces all granted access. On an enabled plugin it
// rebinds (see changeOwnerAccess).
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
	return s.changeOwnerAccess(func(p *pluginRecord) error {
		if p.Launch == nil {
			return model.NewError(model.CodeInvalidInput, "access grants require an external plugin")
		}
		p.Access = a
		return nil
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

// changeOwnerAccess applies one owner change to scope or access. A disabled
// plugin is just rewritten. An enabled plugin is rebound in ONE atomic write:
// the change, a fresh epoch and an "enabled" gap. The running child's binding
// no longer holds (stale_binding), so it stops; a new start gets only the new
// access. The record is always either the old or the new one; a write whose
// confirmation failed is reported by publicationError. The pin is verified
// outside the Store lock first, and the record must not change in between.
func (s PluginFeed) changeOwnerAccess(change func(*pluginRecord) error) error {
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
	if before.ID == "" {
		return model.NewError(model.CodeNotFound, "add the plugin first")
	}
	if before.Launch != nil {
		if err := before.Launch.Verify(); err != nil {
			return err
		}
	}
	return s.write(func(view bus.JournalView) error {
		p, raw, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.ID != before.ID || sha256.Sum256(raw) != prior {
			return model.NewError(model.CodeConflict, "plugin changed while its access was being changed; retry")
		}
		if err := change(&p); err != nil {
			return err
		}
		if err := rebindIfEnabled(view, &p); err != nil {
			return err
		}
		written, err := s.savePlugin(p)
		if err == nil {
			return nil
		}
		return s.publicationError(raw, written, err)
	})
}

// rebindIfEnabled gives an enabled record a fresh epoch and an "enabled" gap,
// so the running child's binding no longer holds. It refuses to leave an
// enabled plugin with no access.
func rebindIfEnabled(view bus.JournalView, p *pluginRecord) error {
	if !p.Enabled {
		return nil
	}
	if !p.mayRun() {
		return model.NewError(model.CodeInvalidInput, "this leaves the plugin no access; use plugins revoke to remove all access")
	}
	head, err := view.EventWindow()
	if err != nil {
		return err
	}
	p.setGap(head, "enabled")
	p.Epoch = model.NewUUID()
	return nil
}

// publicationError reports a failed record write honestly. The write is one
// atomic rename, so after it the record should be the old or the new image.
// Read it again under the same lock and say only what is proven:
//   - the new bytes: written, but the disk did not confirm it;
//   - the old bytes: not written, the previous access stands;
//   - anything else (unreadable, missing, other bytes): unknown, and it may
//     be in effect. Never "unchanged" without the old bytes.
func (s PluginFeed) publicationError(prior, written []byte, err error) error {
	current, readErr := journal.ReadBytes(s.pluginPath(), 16<<10)
	switch {
	case readErr == nil && bytes.Equal(current, written):
		return errors.Join(model.NewError(model.CodeUnavailable, "the access change was written, but the disk did not confirm it; it may be in effect. Check it with: zalki plugins show %s", s.name), err)
	case readErr == nil && bytes.Equal(current, prior):
		return errors.Join(model.NewError(model.CodeUnavailable, "the access change was not written; %s keeps its previous access", s.name), err)
	}
	return errors.Join(model.NewError(model.CodeUnavailable, "the access change may or may not be in effect; check it with: zalki plugins show %s", s.name), err, readErr)
}
