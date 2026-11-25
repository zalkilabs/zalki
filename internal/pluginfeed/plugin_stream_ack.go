package pluginfeed

import (
	"context"
	"slices"

	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func streamAckError() error {
	return model.NewError(model.CodeConflict, "plugin feed ACK was not handed off in this binding")
}

func (s PluginFeed) ackStream(launch *PluginFeedStatus, id string) (string, error) {
	if s.stream == nil || !model.IsCanonicalUUID(id) || !streamFeaturesOf(launch).Feed {
		return "", streamAckError()
	}
	s.stream.mu.Lock()
	defer s.stream.mu.Unlock()
	pendingID := ""
	err := retryStreamWork(s.stream, func() error {
		return s.withStreamStep(context.Background(), launch, true, func(step *streamStep) error {
			var err error
			pendingID, err = s.ackStreamStep(step, id, streamFeaturesOf(launch))
			return err
		})
	})
	if err == nil && s.stream.offer == id && pendingID != id {
		s.stream.offer, s.stream.nextPart = "", 0
	}
	return pendingID, err
}

func (s PluginFeed) ackStreamStep(step *streamStep, id string, features streamFeatures) (string, error) {
	state := &step.state
	if !step.exists || !streamBindingMatches(*state, step.owner, features) {
		return "", streamAckError()
	}
	if id == state.LastAck || slices.Contains(state.Canceled, id) {
		return streamPendingID(state), fsutil.SyncDir(s.pluginDir())
	}
	pending := state.Pending
	if !matchingAttemptedStreamAck(pending, id) {
		return "", streamAckError()
	}
	current, err := s.pendingStreamCurrent(context.Background(), step.view, step.owner, pending, step.budget)
	if err != nil {
		return "", err
	}
	if !current {
		if err := state.queueGap("policy", streamAt(step.head)); err != nil {
			return "", err
		}
		return streamPendingID(state), s.saveStreamState(state)
	}
	if err := commitStreamReceipt(state, pending); err != nil {
		return "", err
	}
	if err := queueStreamPostAck(state, pending, streamAt(step.head)); err != nil {
		return "", err
	}
	return streamPendingID(state), s.saveStreamState(state)
}

func matchingAttemptedStreamAck(pending *streamPending, id string) bool {
	return pending != nil && pending.ID == id && pending.Attempted && pending.EndAttempted
}

func queueStreamPostAck(state *pluginStreamState, pending *streamPending, head pluginPosition) error {
	if pending.ResetAfter != "" {
		if err := state.queueGap(pending.ResetAfter, head); err != nil {
			return err
		}
	}
	if state.ResumeCut {
		state.ResumeCut = false
		return state.queueGap("launch", head)
	}
	return nil
}

func streamPendingID(state *pluginStreamState) string {
	if state.Pending == nil {
		return ""
	}
	return state.Pending.ID
}

func commitStreamReceipt(state *pluginStreamState, pending *streamPending) error {
	state.LastAck, state.Pending = pending.ID, nil
	state.Cursor = pending.After
	switch pending.Kind {
	case "gap":
		state.clearEvidence()
		state.NeedCut = true
	case "event":
		return commitStreamEvidence(state, pending)
	default:
		return journal.ErrInvalidFile
	}
	return nil
}

func commitStreamEvidence(state *pluginStreamState, pending *streamPending) error {
	if pending.SessionSeen != nil {
		state.States[pending.SessionID] = *pending.SessionSeen
	}
	if pending.MessageSeen != nil {
		commitStreamMessageSeen(state, *pending.MessageSeen)
	}
	return nil
}

func commitStreamMessageSeen(state *pluginStreamState, seen streamMessageSeen) {
	state.Seen = slices.DeleteFunc(state.Seen, func(old streamMessageSeen) bool { return old.ID == seen.ID })
	state.Seen = append(state.Seen, seen)
	if len(state.Seen) > streamSeenLimit {
		state.Seen = state.Seen[len(state.Seen)-streamSeenLimit:]
	}
}
