package pluginfeed

import (
	"crypto/sha256"
	"slices"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// PluginCapabilityGrants records separate owner-selected permissions, not effects.
type PluginCapabilityGrants struct {
	Read      []string `json:"read,omitempty"`
	Send      []string `json:"send,omitempty"`
	InboxRead bool     `json:"inbox_read,omitempty"`
	InboxFrom []string `json:"inbox_from,omitempty"`
}

// PluginCapabilities binds recorded permissions to one installation and launch.
// It is not an authentication token and enables no plugin request transport.
type PluginCapabilities struct {
	Version      int    `json:"version"`
	Installation string `json:"installation"`
	PinSHA256    string `json:"pin_sha256"`
	Revision     string `json:"revision"`
	PluginCapabilityGrants
}

func (g PluginCapabilityGrants) empty() bool {
	return len(g.Read)+len(g.Send)+len(g.InboxFrom) == 0 && !g.InboxRead
}

func (c *PluginCapabilities) clone() *PluginCapabilities {
	if c == nil {
		return nil
	}
	result := *c
	result.Read = slices.Clone(c.Read)
	result.Send = slices.Clone(c.Send)
	result.InboxFrom = slices.Clone(c.InboxFrom)
	return &result
}

func (s pluginFiles) checkCapabilityGrants(g PluginCapabilityGrants) error {
	for _, ids := range [][]string{g.Read, g.Send, g.InboxFrom} {
		if err := s.checkPluginGrants(ids); err != nil {
			return err
		}
		for _, id := range ids {
			if strings.HasPrefix(id, "plugin:") {
				return model.NewError(model.CodeInvalidInput, "capability targets require native session addresses")
			}
		}
	}
	ids := slices.Concat(g.Read, g.Send, g.InboxFrom)
	slices.Sort(ids)
	return s.checkPluginGrants(slices.Compact(ids))
}

func (s pluginFiles) checkCapabilities(c *PluginCapabilities) error {
	if c == nil {
		return nil
	}
	if c.Version != 1 || !model.IsCanonicalUUID(c.Installation) || !validPluginSHA(c.PinSHA256) || !model.IsCanonicalUUID(c.Revision) || c.empty() {
		return journal.ErrInvalidBatch
	}
	return s.checkCapabilityGrants(c.PluginCapabilityGrants)
}

// GrantPluginCapabilities replaces all recorded permissions while disabled.
// Pin verification runs outside Store flock; the complete record is rechecked.
func (s PluginFeed) GrantPluginCapabilities(g PluginCapabilityGrants) error {
	if err := s.checkName(); err != nil {
		return err
	}
	for _, ids := range []*[]string{&g.Read, &g.Send, &g.InboxFrom} {
		*ids = slices.Clone(*ids)
		slices.Sort(*ids)
	}
	if g.empty() {
		return model.NewError(model.CodeInvalidInput, "select explicit permissions or use plugins revoke")
	}
	if err := s.checkCapabilityGrants(g); err != nil {
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
	if before.ID == "" {
		return model.NewError(model.CodeNotFound, "add the plugin first")
	}
	if before.Enabled {
		return model.NewError(model.CodeConflict, "disable the plugin before granting capabilities")
	}
	if before.Launch == nil {
		return model.NewError(model.CodeInvalidInput, "capability grants require an external plugin")
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
			return model.NewError(model.CodeConflict, "plugin changed before granting capabilities; retry")
		}
		p.Capabilities = &PluginCapabilities{Version: 1, Installation: p.ID, PinSHA256: p.Launch.PinSHA256, Revision: model.NewUUID(), PluginCapabilityGrants: g}
		_, err = s.savePlugin(p)
		return err
	})
}

// RevokePluginCapabilities removes permissions and atomically disables the host.
// This preserves the existing metadata ACK fence without a new receipt scheme.
func (s PluginFeed) RevokePluginCapabilities() error {
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
		p.Capabilities, p.Enabled = nil, false
		_, err = s.savePlugin(p)
		return err
	})
}
