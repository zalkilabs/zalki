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
	"context"
	"encoding/json"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// PluginFeed provides the local plugin feed using its Store's journal lock.
type PluginFeed struct {
	store  bus.Store
	stream *streamRuntime
	pluginFiles
}

// pluginFiles contains the paths and selectors used inside a journal callback.
type pluginFiles struct{ root, name string }

// NewPluginFeed returns the named installation feed; operations reject invalid names.
func NewPluginFeed(store bus.Store, name string) PluginFeed {
	return PluginFeed{store: store, stream: newStreamRuntime(), pluginFiles: pluginFiles{root: store.Dir(), name: name}}
}

// PluginDelivery contains metadata only. Its private receipt stays with the host;
// the child returns just ID, never a position supplied by executable code.
type PluginDelivery struct {
	Frames []json.RawMessage `json:"-"`
	ID     string            `json:"id"`
	Gap    string            `json:"gap,omitempty"`
	Event  *model.Event      `json:"event,omitempty"`
}

// NextPluginFeed prepares a bounded immutable offer, or preflights one exact
// 1-based part immediately before handoff. Inspection (empty ID/part zero)
// exposes no payload and never creates attempt evidence. Callers write only
// the freshly returned part, after this method releases the journal lock.
func (s PluginFeed) NextPluginFeed(ctx context.Context, launch *PluginFeedStatus, expectedID string, part int) (PluginDelivery, error) {
	return s.nextStream(ctx, launch, expectedID, part)
}

// feedAllowed is checked under the same lock that picks the event: the
// plugin is enabled, this exact launch still holds, and it has status access.
func feedAllowed(p pluginRecord, launch *PluginFeedStatus) (bool, error) {
	switch {
	case !p.Enabled:
		return false, model.NewError(model.CodeUnauthorized, "plugin is disabled")
	case launchBindingCode(p, launch) != "ok":
		return false, model.NewError(model.CodeUnauthorized, "plugin launch is stale")
	}
	return p.Access.Status, nil
}

// AckPluginFeed commits only the exact durable handoff-attempted receipt of
// this captured launch. Its pending ID tells the host whether a late canceled
// ACK left a different offer outstanding; it never refreshes an ACK timer.
func (s PluginFeed) AckPluginFeed(launch *PluginFeedStatus, id string) (string, error) {
	return s.ackStream(launch, id)
}

func addressKind(address string) string {
	kind, _, _ := strings.Cut(address, ":")
	return kind + ":"
}
