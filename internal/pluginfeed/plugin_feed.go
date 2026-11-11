// Package pluginfeed owns plugin journal state, separate from native messaging
// in bus and process/build work in plugins. Its ownership map is:
//
//	Installations and records  identity, capabilities, grants and pinned launches
//	Feed replay                positions, gaps and delivery acknowledgements
//	Inbox                      admission, reservation indexes and read audit
//	Installer commit           one atomic enabled-record publication under the Store lock
//	Child lease                read-only fd 3 lease shared with the child until exit
//
// Plugin state uses the core journal lock and JournalView boundary; bus must not
// import pluginfeed. The CLI injects inbox admission and reservation into bus.
// cmd/zalki owns install prompts, cancellation and the order of install steps.
// internal/plugins owns source snapshots, tool resolution, builds and build logs.
// This package owns install staging, pin verification and the record commit point.
package pluginfeed

import (
	"crypto/sha256"
	"slices"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// PluginFeed provides the local plugin feed using its Store's journal lock.
type PluginFeed struct {
	store bus.Store
	pluginFiles
}

// pluginFiles contains the paths and selectors used inside a journal callback.
type pluginFiles struct{ root, name string }

// NewPluginFeed returns the named installation feed; operations reject invalid names.
func NewPluginFeed(store bus.Store, name string) PluginFeed {
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

func (p *pluginRecord) setGap(head bus.EventWindow, reason string) {
	p.Position = pluginPosition{head.Epoch, head.Last}
	p.GapID, p.GapReason = model.NewUUID(), reason
}

func (p pluginRecord) lost(head bus.EventWindow) string {
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
	err := s.write(func(view bus.JournalView) error {
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

func (s pluginFiles) scanPlugin(view bus.JournalView, p pluginRecord, raw []byte, head bus.EventWindow) (PluginDelivery, error) {
	position := p.Position.Sequence
	for scanned := 0; position < head.Last && scanned < 64; scanned++ {
		position++
		event, err := view.EventAt(head.Epoch, position)
		if err != nil {
			return PluginDelivery{}, err
		}
		if offered, ok := feedView(p.Sessions, event); ok {
			return pluginOffer(event.ID, "", &offered, raw, pluginPosition{head.Epoch, position}), nil
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

// feedView returns the event as a plugin with these feed sessions may see it.
// A session event needs its session. A message event needs its sender or its
// recipient; the other party is reduced to "kind:". reply_to is kept: bus
// writes a reply only between its parent's parties (checkReplyParties), so the
// parent is eligible for the same feed set.
func feedView(sessions []string, event model.Event) (model.Event, bool) {
	if event.Message == nil {
		return event, slices.Contains(sessions, event.Session.ID)
	}
	m := *event.Message
	from, to := slices.Contains(sessions, m.From), slices.Contains(sessions, m.To)
	if !from && !to {
		return model.Event{}, false
	}
	if !from {
		m.From = addressKind(m.From)
	}
	if !to {
		m.To = addressKind(m.To)
	}
	event.Message = &m
	return event, true
}

func addressKind(address string) string {
	kind, _, _ := strings.Cut(address, ":")
	return kind + ":"
}
