package pluginfeed

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const maxPluginGrants = 16

type pluginPosition struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence,string"`
}

type pluginRecord struct {
	ID           string                `json:"id"`
	Name         string                `json:"name"`
	Launch       *PluginLaunch         `json:"launch,omitempty"`
	Enabled      bool                  `json:"enabled"`
	Sessions     []string              `json:"sessions"`
	Position     pluginPosition        `json:"position"`
	GapID        string                `json:"gap_id,omitempty"`
	GapReason    string                `json:"gap_reason,omitempty"`
	Capabilities *PluginCapabilities   `json:"capabilities,omitempty"`
	Install      *PluginInstallReceipt `json:"install,omitempty"`
	// Epoch is new on every enable. A running child keeps the epoch of its
	// launch, so it never adopts grants that changed after it started.
	Epoch string `json:"epoch,omitempty"`
}

// PluginFeedStatus describes a named installation, without claiming a live child.
type PluginFeedStatus struct {
	Launch       *PluginLaunch         `json:"launch,omitempty"`
	ID           string                `json:"id,omitempty"`
	Name         string                `json:"plugin"`
	DataDir      string                `json:"data_dir"`
	Enabled      bool                  `json:"enabled"`
	Sessions     []string              `json:"sessions"`
	GapPending   bool                  `json:"gap_pending"`
	LastGap      string                `json:"last_gap,omitempty"`
	Capabilities *PluginCapabilities   `json:"capabilities,omitempty"`
	Install      *PluginInstallReceipt `json:"install,omitempty"`
	Epoch        string                `json:"epoch,omitempty"`
}

func (s pluginFiles) pluginPath() string { return filepath.Join(s.pluginDir(), s.name+".json") }

func (s pluginFiles) checkPluginGrants(ids []string) error {
	if len(ids) > maxPluginGrants {
		return model.NewError(model.CodeInvalidInput, "plugin permits at most 16 explicit session grants")
	}
	for i, id := range ids {
		if err := model.ValidateSessionAddress(id); err != nil {
			return err
		}
		if model.IsPluginInboxAddress(id) {
			return model.NewError(model.CodeInvalidInput, "plugin grants require native session addresses")
		}
		if i > 0 && ids[i-1] >= id {
			return model.NewError(model.CodeInvalidInput, "plugin grants must be unique and sorted")
		}
	}
	return nil
}

// decodePluginRecord decodes strictly. A record of an older Zalki gives
// ErrOldPluginRecord with the fix, and p keeps only its installation ID.
func decodePluginRecord(name string, raw []byte, p *pluginRecord) error {
	err := bus.DecodeStrictRecordJSON(raw, p)
	if err == nil {
		return nil
	}
	if old := oldPluginRecord(name, raw); old != nil {
		*p = pluginRecord{ID: oldPluginID(raw)}
		return old
	}
	return err
}

func (s pluginFiles) loadPlugin() (pluginRecord, []byte, error) {
	var p pluginRecord
	if err := s.checkName(); err != nil {
		return p, nil, err
	}
	if err := pluginDirectory(s.pluginDir(), false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil, nil
		}
		return p, nil, err
	}
	raw, err := journal.ReadBytes(s.pluginPath(), 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil, nil
	}
	if err != nil {
		return p, nil, err
	}
	if err := decodePluginRecord(s.name, raw, &p); err != nil {
		return p, nil, err
	}
	if !p.validIdentityAndLaunch(s.name) {
		return p, nil, journal.ErrInvalidBatch
	}
	if err := s.checkPluginGrants(p.Sessions); err != nil {
		return p, nil, err
	}
	if err := s.checkCapabilities(p.Capabilities); err != nil {
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
		(p.Position.Epoch != "" && !model.IsCanonicalUUID(p.Position.Epoch)) ||
		(p.GapID != "" && (!model.IsCanonicalUUID(p.GapID) || p.GapReason == ""))
}

func (s pluginFiles) savePlugin(p pluginRecord) ([]byte, error) {
	raw, err := journal.EncodeJSON(p, 16<<10)
	if err != nil {
		return nil, err
	}
	return raw, journal.WriteAtomic(s.pluginPath(), raw)
}

// ConfigurePluginFeed replaces explicit grants only while execution is disabled.
func (s PluginFeed) ConfigurePluginFeed(ids []string) error {
	if err := s.checkName(); err != nil {
		return err
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	if err := s.checkPluginGrants(ids); err != nil {
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
		if p.Enabled {
			return model.NewError(model.CodeConflict, "disable the plugin before changing its grants")
		}
		_, err = s.savePlugin(pluginRecord{ID: p.ID, Name: p.Name, Sessions: ids, Launch: p.Launch, Capabilities: p.Capabilities, Install: p.Install})
		return err
	})
}

// EnablePluginFeed starts at the current end; it never launches executable code.
func (s PluginFeed) EnablePluginFeed(enabled bool) error {
	if err := s.checkName(); err != nil {
		return err
	}
	var launch *PluginLaunch
	var prior [32]byte
	if enabled {
		var err error
		launch, prior, err = s.preparePluginPin()
		if err != nil {
			return err
		}
	}
	return s.enablePluginFeed(enabled, launch, prior)
}

func (s PluginFeed) enablePluginFeed(enabled bool, launch *PluginLaunch, prior [32]byte) error {
	return s.write(func(view bus.JournalView) error {
		p, raw, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.ID == "" {
			return model.NewError(model.CodeNotFound, "add the plugin first")
		}
		// This also backfills the UUID hint for existing enabled installations.
		if err := syncPluginIdentity(s.pluginFiles, p.ID, enabled); err != nil {
			return err
		}
		if p.Enabled == enabled {
			return journal.SyncDir(s.pluginDir())
		}
		if enabled {
			if err := p.applyLaunchPin(launch, prior, raw); err != nil {
				return err
			}
			if len(p.Sessions) == 0 {
				return model.NewError(model.CodeInvalidInput, "configure explicit session grants first")
			}
			head, err := view.EventWindow()
			if err != nil {
				return err
			}
			p.setGap(head, "enabled")
			p.Epoch = model.NewUUID()
		}
		p.Enabled = enabled
		_, err = s.savePlugin(p)
		return err
	})
}

// PluginStatus is owner-facing; journal positions are not part of child frames.
func (s PluginFeed) PluginStatus() (PluginFeedStatus, error) {
	if err := s.checkName(); err != nil {
		return PluginFeedStatus{}, err
	}
	var status PluginFeedStatus
	err := bus.WithJournalRead(s.store, func(bus.JournalView) error {
		p, _, err := s.loadPlugin()
		status = PluginFeedStatus{Launch: p.Launch, ID: p.ID, Name: s.name, DataDir: s.pluginDataPath(), Enabled: p.Enabled, Sessions: slices.Clone(p.Sessions), GapPending: p.GapID != "", LastGap: p.GapReason, Capabilities: p.Capabilities.clone(), Install: p.Install, Epoch: p.Epoch}
		return err
	})
	return status, err
}

func (p pluginRecord) validIdentityAndLaunch(name string) bool {
	if !model.IsCanonicalUUID(p.ID) || p.Name != name || (p.Install != nil && (p.Launch == nil || !p.Install.valid())) {
		return false
	}
	// Every installation runs an external, pinned executable.
	return p.Launch != nil && p.Launch.validate() == nil && validPluginSHA(p.Launch.SHA256) && validPluginSHA(p.Launch.PinSHA256)
}

func (p *pluginRecord) applyLaunchPin(launch *PluginLaunch, prior [32]byte, raw []byte) error {
	if p.Launch == nil {
		return nil
	}
	if launch == nil || sha256.Sum256(raw) != prior {
		return model.NewError(model.CodeConflict, "plugin changed while pinning; retry enable")
	}
	p.Launch = launch
	return nil
}
