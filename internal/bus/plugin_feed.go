package bus

import (
	"crypto/sha256"
	"slices"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// PluginFeed provides the local plugin feed using its Store's journal lock.
type PluginFeed struct {
	store Store
	pluginFiles
}

// pluginFiles contains the paths and selectors used inside a journal callback.
type pluginFiles struct{ root, name string }

// NewPluginFeed returns the named installation feed; operations reject invalid names.
func NewPluginFeed(store Store, name string) PluginFeed {
	return PluginFeed{store: store, pluginFiles: pluginFiles{root: store.Dir, name: name}}
}

// PluginDelivery contains metadata only. Its private receipt stays with the host;
// the child returns just ID, never a position supplied by executable code.
type PluginDelivery struct {
	ID    string       `json:"id"`
	Gap   string       `json:"gap,omitempty"`
	Event *model.Event `json:"event,omitempty"`
	prior [32]byte
	after pluginPosition
	offer string
}

func (p *pluginRecord) setGap(head EventWindow, reason string) {
	p.Position = pluginPosition{head.Epoch, head.Last}
	p.GapID, p.GapReason = newID(), reason
}

func (p pluginRecord) lost(head EventWindow) string {
	// An empty-stream position binds its first real epoch without skipping events.
	if (p.Position.Epoch != "" && p.Position.Epoch != head.Epoch) || p.Position.Sequence > head.Last {
		return "epoch"
	}
	if head.Count != 0 && p.Position.Sequence < head.First-1 {
		return "retention"
	}
	return ""
}

func pluginOffer(id, gap string, event *model.Event, raw []byte, after pluginPosition) PluginDelivery {
	return PluginDelivery{ID: id, Gap: gap, Event: event, prior: sha256.Sum256(raw), after: after, offer: id}
}

// NextPluginFeed offers one event or a durable gap. At most 64 records (256KiB)
// are examined per call. No plugin work or callbacks execute under this lock.
func (s PluginFeed) NextPluginFeed() (PluginDelivery, error) {
	var delivery PluginDelivery
	err := s.write(func(view JournalView) error {
		p, raw, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if !p.Enabled {
			return model.NewError(model.CodeUnauthorized, "plugin is disabled")
		}
		head, err := view.EventWindow()
		if err != nil {
			return err
		}
		// Keep the offered gap and its watermark until ACK, even after later loss.
		if reason := p.lost(head); p.GapID == "" && reason != "" {
			p.setGap(head, reason)
			raw, err = s.savePlugin(p)
			if err != nil {
				return err
			}
		}
		if p.GapID != "" {
			// A previous rename may be visible after its directory sync failed.
			if err := journal.SyncDir(s.pluginDir()); err != nil {
				return err
			}
			delivery = pluginOffer(p.GapID, p.GapReason, nil, raw, p.Position)
			return nil
		}
		delivery, err = s.scanPlugin(view, p, raw, head)
		return err
	})
	return delivery, err
}

func (s pluginFiles) scanPlugin(view JournalView, p pluginRecord, raw []byte, head EventWindow) (PluginDelivery, error) {
	position := p.Position.Sequence
	for scanned := 0; position < head.Last && scanned < 64; scanned++ {
		position++
		event, err := view.EventAt(head.Epoch, position)
		if err != nil {
			return PluginDelivery{}, err
		}
		if slices.Contains(p.Sessions, event.Session.ID) {
			return pluginOffer(event.ID, "", &event, raw, pluginPosition{head.Epoch, position}), nil
		}
	}
	if position != p.Position.Sequence {
		p.Position = pluginPosition{head.Epoch, position}
		if _, err := s.savePlugin(p); err != nil {
			return PluginDelivery{}, err
		}
	}
	return PluginDelivery{}, nil
}

// AckPluginFeed persists only a receipt actually offered by this core API.
// Restart before ACK replays; an old receipt cannot cross disable/reconfigure.
func (s PluginFeed) AckPluginFeed(delivery PluginDelivery) error {
	return s.locked(func() error {
		p, raw, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if !p.Enabled || delivery.ID == "" || delivery.ID != delivery.offer || sha256.Sum256(raw) != delivery.prior {
			return model.NewError(model.CodeConflict, "plugin delivery is stale or was not offered")
		}
		p.Position, p.GapID = delivery.after, ""
		_, err = s.savePlugin(p)
		return err
	})
}
