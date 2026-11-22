package pluginfeed

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type streamRuntime struct {
	mu               sync.Mutex
	run              string
	now              func() time.Time
	budget           func(context.Context) *streamBudget
	retryAt          time.Time
	backoff          int
	offer            string
	nextPart         int
	controlID        string
	integrityEpisode bool
	integrityPending *streamIntegrity
	waitingPending   *streamWaiting
	// retryWork is true while a stream step that only missed its work budget
	// will be run again; such a miss must not reset the stream.
	retryWork bool
}

func newStreamRuntime() *streamRuntime {
	return &streamRuntime{run: model.NewUUID(), now: time.Now, budget: newStreamBudget}
}

func streamFeaturesOf(launch *PluginFeedStatus) streamFeatures {
	if launch == nil {
		return streamFeatures{}
	}
	return streamFeatures{Feed: launch.FeedFeatures.Feed}
}

func streamAt(head bus.EventWindow) pluginPosition {
	return pluginPosition{Epoch: head.Epoch, Sequence: head.Last}
}

func streamLost(position pluginPosition, head bus.EventWindow) string {
	if (position.Epoch != "" && position.Epoch != head.Epoch) || position.Sequence > head.Last {
		return "epoch"
	}
	if head.Count != 0 && position.Sequence < head.First-1 {
		return "retention"
	}
	return ""
}

func (state *pluginStreamState) queueGap(reason string, position pluginPosition) error {
	if state.Pending != nil && state.Pending.Kind == "gap" {
		return nil
	}
	state.cancelPending()
	state.clearEvidence()
	raw, err := encodeStreamFrame(streamFrame{ID: model.NewUUID(), Gap: reason})
	if err != nil {
		return err
	}
	var frame streamFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return err
	}
	state.Pending = &streamPending{ID: frame.ID, Kind: "gap", Frames: []json.RawMessage{raw}, After: position, Guards: map[string]streamDependency{}}
	state.LastGap = reason
	state.NeedCut = true
	return nil
}

func (s PluginFeed) pendingStreamCurrent(ctx context.Context, view bus.JournalView, owner pluginRecord, pending *streamPending, budget *streamBudget) (bool, error) {
	if pending == nil || pending.Kind == "gap" {
		return true, nil
	}
	ids := make([]string, 0, len(pending.Guards))
	for id := range pending.Guards {
		ids = append(ids, id)
	}
	collection, err := collectStreamMembers(ctx, view, owner, ids, budget)
	if err != nil {
		return false, err
	}
	if len(collection.Records) != len(ids) {
		return false, journal.ErrInvalidFile
	}
	for _, row := range collection.Records {
		original := pending.Guards[row.ID]
		if pending.Retirement != nil {
			if !streamRetirementCurrent(pending, row) {
				return false, nil
			}
			continue
		}
		if original.Guard != row.Guard || original.Class != row.Class {
			return false, nil
		}
	}
	return true, nil
}

func (s PluginFeed) captureStreamCut(state *pluginStreamState, head bus.EventWindow) {
	state.clearEvidence()
	state.NeedCut = false
	state.Cursor = streamAt(head)
	s.stream.backoff = 0
	state.Backoff = 0
	s.stream.integrityEpisode = false
}
