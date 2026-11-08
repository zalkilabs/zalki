package bus

import (
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const maxPluginGrants = 16

type pluginPosition struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence,string"`
}

type pluginRecord struct {
	Enabled   bool           `json:"enabled"`
	Sessions  []string       `json:"sessions"`
	Position  pluginPosition `json:"position"`
	GapID     string         `json:"gap_id,omitempty"`
	GapReason string         `json:"gap_reason,omitempty"`
}

// PluginFeedStatus describes the single local reference installation.
type PluginFeedStatus struct {
	Enabled    bool     `json:"enabled"`
	Sessions   []string `json:"sessions"`
	GapPending bool     `json:"gap_pending"`
	LastGap    string   `json:"last_gap,omitempty"`
}

func (s PluginFeed) pluginPath() string { return filepath.Join(s.store.Dir, "plugin.json") }

func (s PluginFeed) checkPluginGrants(ids []string) error {
	if len(ids) > maxPluginGrants {
		return model.NewError(model.CodeInvalidInput, "plugin permits at most 16 explicit session grants")
	}
	for i, id := range ids {
		if _, err := s.store.sessionRecordPath(id); err != nil {
			return err
		}
		if i > 0 && ids[i-1] >= id {
			return model.NewError(model.CodeInvalidInput, "plugin grants must be unique and sorted")
		}
	}
	return nil
}

func (s PluginFeed) loadPlugin() (pluginRecord, []byte, error) {
	var p pluginRecord
	raw, err := journal.ReadBytes(s.pluginPath(), 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil, nil
	}
	if err != nil {
		return p, nil, err
	}
	if err := decodeEventJSON(raw, &p); err != nil {
		return p, nil, err
	}
	if err := s.checkPluginGrants(p.Sessions); err != nil {
		return p, nil, err
	}
	if (p.Enabled && (len(p.Sessions) == 0 || p.GapReason == "")) || p.invalidPosition() {
		return p, nil, journal.ErrInvalidBatch
	}
	switch p.GapReason {
	case "", "enabled", "retention", "epoch":
		return p, raw, nil
	default:
		return p, nil, journal.ErrInvalidBatch
	}
}

func (p pluginRecord) invalidPosition() bool {
	return (p.Position.Epoch == "" && p.Position.Sequence != 0) ||
		(p.Position.Epoch != "" && !canonicalEventID(p.Position.Epoch)) ||
		(p.GapID != "" && (!canonicalEventID(p.GapID) || p.GapReason == ""))
}

func (s PluginFeed) savePlugin(p pluginRecord) ([]byte, error) {
	raw, err := journal.EncodeJSON(p, 16<<10)
	if err != nil {
		return nil, err
	}
	return raw, journal.WriteAtomic(s.pluginPath(), raw)
}

// ConfigurePluginFeed replaces explicit grants only while execution is disabled.
func (s PluginFeed) ConfigurePluginFeed(ids []string) error {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	if err := s.checkPluginGrants(ids); err != nil {
		return err
	}
	return s.store.locked(func() error {
		p, _, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.Enabled {
			return model.NewError(model.CodeConflict, "disable the plugin before changing its grants")
		}
		_, err = s.savePlugin(pluginRecord{Sessions: ids})
		return err
	})
}

// EnablePluginFeed starts at the current end; it never launches executable code.
func (s PluginFeed) EnablePluginFeed(enabled bool) error {
	return s.store.locked(func() error {
		p, raw, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.Enabled == enabled {
			if raw != nil {
				return journal.SyncDir(s.store.Dir)
			}
			return nil
		}
		if enabled {
			if len(p.Sessions) == 0 {
				return model.NewError(model.CodeInvalidInput, "configure explicit session grants first")
			}
			head, err := s.pluginHead()
			if err != nil {
				return err
			}
			p.setGap(head, "enabled")
		}
		p.Enabled = enabled
		_, err = s.savePlugin(p)
		return err
	})
}

// PluginStatus is owner-facing; journal positions are not part of child frames.
func (s PluginFeed) PluginStatus() (PluginFeedStatus, error) {
	var status PluginFeedStatus
	err := s.store.readLocked(func() error {
		p, _, err := s.loadPlugin()
		status = PluginFeedStatus{p.Enabled, slices.Clone(p.Sessions), p.GapID != "", p.GapReason}
		return err
	})
	return status, err
}
