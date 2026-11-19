package pluginfeed

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/zalkilabs/zalki/internal/bus"
)

type streamDependency struct {
	Guard streamGuard `json:"guard"`
	Class string      `json:"class,omitempty"`
}

type streamCollection struct {
	Records []streamRow `json:"records"`
}

func collectStreamMembers(ctx context.Context, view bus.JournalView, p pluginRecord, targets []string, budget *streamBudget) (streamCollection, error) {
	var result streamCollection
	raw, err := bus.PluginMembers(ctx, view, p.Name, p.Scope, p.Access.Status, targets, budget.charge)
	if err != nil {
		return result, err
	}
	err = decodeStreamJSON(raw, &result)
	return result, err
}

func streamDescriptor(pending *streamPending, part int) PluginDelivery {
	if pending == nil {
		return PluginDelivery{}
	}
	delivery := PluginDelivery{ID: pending.ID, Frames: make([]json.RawMessage, len(pending.Frames))}
	if len(pending.Frames) == 1 {
		var frame streamFrame
		if json.Unmarshal(pending.Frames[0], &frame) == nil {
			delivery.Gap = frame.Gap
			if part > 0 {
				delivery.Event = frame.Event
			}
		}
	}
	// Inspect descriptors contain no handoff bytes. Only the freshly authorized
	// requested part is populated; cached/fabricated fields cannot authorize it.
	if part > 0 && part <= len(pending.Frames) {
		delivery.Frames[part-1] = slices.Clone(pending.Frames[part-1])
	}
	return delivery
}
