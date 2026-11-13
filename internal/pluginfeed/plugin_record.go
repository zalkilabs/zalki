package pluginfeed

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"

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
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Launch  *PluginLaunch `json:"launch,omitempty"`
	Enabled bool          `json:"enabled"`
	// Scope "all" gives standard access to every live session; any other
	// value is none.
	Scope     string                `json:"scope"`
	Access    PluginAccess          `json:"access"`
	Position  pluginPosition        `json:"position"`
	GapID     string                `json:"gap_id,omitempty"`
	GapReason string                `json:"gap_reason,omitempty"`
	Install   *PluginInstallReceipt `json:"install,omitempty"`
	// Revision is pluginRevision of this record, checked on every load.
	Revision string `json:"revision"`
	// Epoch is new on every enable. A running child keeps the epoch of its
	// launch, so it never adopts grants that changed after it started.
	Epoch string `json:"epoch,omitempty"`
}

// PluginFeedStatus describes a named installation, without claiming a live child.
type PluginFeedStatus struct {
	Launch     *PluginLaunch         `json:"launch,omitempty"`
	ID         string                `json:"id,omitempty"`
	Name       string                `json:"plugin"`
	DataDir    string                `json:"data_dir"`
	Enabled    bool                  `json:"enabled"`
	Scope      string                `json:"scope"`
	Access     PluginAccess          `json:"access"`
	Revision   string                `json:"revision,omitempty"`
	GapPending bool                  `json:"gap_pending"`
	LastGap    string                `json:"last_gap,omitempty"`
	Install    *PluginInstallReceipt `json:"install,omitempty"`
	Epoch      string                `json:"epoch,omitempty"`
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
	// An old shape is named first: some old records also decode cleanly.
	if old := oldPluginRecord(name, raw); old != nil {
		*p = pluginRecord{ID: oldPluginID(raw)}
		return old
	}
	return bus.DecodeStrictRecordJSON(raw, p)
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
	if err := s.checkAccess(p.Access); err != nil {
		return p, nil, err
	}
	if p.Revision != pluginRevision(p) {
		return p, nil, journal.ErrInvalidBatch // a hand edit cannot keep an old binding
	}
	if (p.Enabled && (!p.mayRun() || p.GapReason == "")) || p.invalidPosition() {
		return p, nil, journal.ErrInvalidBatch
	}
	switch p.GapReason {
	case "", "enabled", "retention", "epoch":
		return p, raw, nil
	default:
		return p, nil, journal.ErrInvalidBatch
	}
}

// mayRun reports whether the record may be enabled: it has some access, or
// it was installed declaring send or read and the owner chose no session for
// them (installed with no grants; the plugin then gets no capability).
func (p pluginRecord) mayRun() bool {
	if !p.Access.empty() {
		return true
	}
	if p.Install == nil {
		return false
	}
	declared, ok := decodeDeclaredAccess(p.Install.Access)
	return ok && declared.extended()
}

func (p pluginRecord) invalidPosition() bool {
	return (p.Position.Epoch == "" && p.Position.Sequence != 0) ||
		(p.Position.Epoch != "" && !model.IsCanonicalUUID(p.Position.Epoch)) ||
		(p.GapID != "" && (!model.IsCanonicalUUID(p.GapID) || p.GapReason == ""))
}

func (s pluginFiles) savePlugin(p pluginRecord) ([]byte, error) {
	p.Revision = pluginRevision(p)
	raw, err := journal.EncodeJSON(p, 16<<10)
	if err != nil {
		return nil, err
	}
	return raw, journal.WriteAtomic(s.pluginPath(), raw)
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
			if !p.mayRun() {
				return model.NewError(model.CodeInvalidInput, "grant some access first")
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
		status = PluginFeedStatus{Launch: p.Launch, ID: p.ID, Name: s.name, DataDir: s.pluginDataPath(), Enabled: p.Enabled, Scope: p.Scope, Access: p.Access.clone(), Revision: p.Revision, GapPending: p.GapID != "", LastGap: p.GapReason, Install: p.Install, Epoch: p.Epoch}
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
