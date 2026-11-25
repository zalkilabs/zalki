package pluginfeed

import (
	"context"
	"errors"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type streamStep struct {
	view   bus.JournalView
	owner  pluginRecord
	head   bus.EventWindow
	budget *streamBudget
	state  pluginStreamState
	exists bool
	start  pluginPosition
}

func (s PluginFeed) withStreamStep(ctx context.Context, launch *PluginFeedStatus, ack bool, fn func(*streamStep) error) error {
	if ctx == nil || s.stream == nil {
		return journal.ErrInvalidFile
	}
	return s.write(func(view bus.JournalView) error {
		step := &streamStep{view: view, budget: s.stream.budget(ctx)}
		owner, _, err := s.loadPluginUsing(step.budget.read)
		if err != nil {
			return err
		}
		allowed, err := streamLaunchAllowed(owner, launch, ack)
		if err != nil || !allowed {
			return err
		}
		step.owner = owner
		if _, err := collectStreamMembers(ctx, view, owner, []string{}, step.budget); err != nil {
			return err
		}
		step.head, err = view.EventWindow()
		if err != nil {
			return err
		}
		step.state, step.exists, err = s.loadStreamState(step.budget)
		if err != nil {
			return err
		}
		step.start = step.state.Cursor
		return fn(step)
	})
}

func streamLaunchAllowed(owner pluginRecord, launch *PluginFeedStatus, ack bool) (bool, error) {
	if ack {
		if launch == nil || owner.ID != launch.ID || owner.Launch == nil {
			return false, streamAckError()
		}
		if owner.Enabled && launchBindingCode(owner, launch) != "ok" {
			return false, streamAckError()
		}
	}
	return feedAllowed(owner, launch)
}

func streamBindingMatches(state pluginStreamState, owner pluginRecord, features streamFeatures) bool {
	return state.Installation == owner.ID && state.Enable == owner.Epoch && state.Revision == owner.Revision && state.Features == features
}

func (s PluginFeed) nextStream(ctx context.Context, launch *PluginFeedStatus, expectedID string, part int) (PluginDelivery, error) {
	if !validStreamOfferRequest(s.stream, ctx, expectedID, part) {
		return PluginDelivery{}, journal.ErrInvalidFile
	}
	s.stream.mu.Lock()
	defer s.finishStreamStep()
	features := streamFeaturesOf(launch)
	if !features.Feed {
		return PluginDelivery{}, nil
	}
	if s.stream.now().Before(s.stream.retryAt) && (part <= 0 || expectedID != s.stream.controlID) {
		if part > 0 {
			return PluginDelivery{}, model.NewError(model.CodeUnavailable, "plugin feed is waiting for a bounded resource retry")
		}
		return PluginDelivery{}, nil
	}
	var delivery PluginDelivery
	err := retryStreamWork(s.stream, func() error {
		return s.withStreamStep(ctx, launch, false, func(step *streamStep) error {
			var err error
			delivery, err = s.nextStreamStep(ctx, step, features, expectedID, part)
			return err
		})
	})
	if errors.Is(err, errStreamWork) {
		s.stream.backoff = min(max(s.stream.backoff*2, 1), 60)
		s.stream.retryAt = s.stream.now().Add(time.Duration(s.stream.backoff) * time.Second)
		s.stream.waitingPending = &streamWaiting{reason: "resource", delay: s.stream.backoff}
		return PluginDelivery{}, model.NewError(model.CodeUnavailable, "plugin feed work is temporarily unavailable")
	}
	if err == nil && delivery.Gap != "" {
		s.stream.controlID = delivery.ID
	}
	return delivery, err
}

// streamWorkAttempts is how often one stream call runs its step while the step
// only misses the 50 ms work budget. A miss on a busy machine is usually
// short, and a failed try publishes nothing, so another try is safe. Each try
// opens and closes its own journal step.
const streamWorkAttempts = 3

// retryStreamWork runs step until it does not fail with errStreamWork, at most
// streamWorkAttempts times. Only on the last try may the step reset the stream.
func retryStreamWork(runtime *streamRuntime, step func() error) error {
	defer func() { runtime.retryWork = false }()
	var err error
	for try := 1; try <= streamWorkAttempts; try++ {
		runtime.retryWork = try < streamWorkAttempts
		if err = step(); !errors.Is(err, errStreamWork) {
			return err
		}
	}
	return err
}

func validStreamOfferRequest(runtime *streamRuntime, ctx context.Context, id string, part int) bool {
	if runtime == nil || ctx == nil || part < 0 {
		return false
	}
	if part == 0 {
		return id == ""
	}
	return model.IsCanonicalUUID(id)
}

func (s PluginFeed) nextStreamStep(ctx context.Context, step *streamStep, features streamFeatures, expectedID string, part int) (PluginDelivery, error) {
	if err := s.prepareStreamLaunch(step, features); err != nil {
		return PluginDelivery{}, err
	}
	if err := s.advanceStreamForOffer(ctx, step); err != nil {
		return PluginDelivery{}, err
	}
	if step.state.Pending == nil {
		return PluginDelivery{}, s.saveStreamState(&step.state)
	}
	if part > 0 && expectedID == step.state.Pending.ID {
		if err := s.attemptStreamPart(ctx, step, part); err != nil {
			return PluginDelivery{}, err
		}
		if expectedID == step.state.Pending.ID {
			return streamDescriptor(step.state.Pending, part), nil
		}
	}
	// Preparation never exposes bytes or creates restart/ACK evidence.
	if err := s.saveStreamState(&step.state); err != nil {
		return PluginDelivery{}, err
	}
	return streamDescriptor(step.state.Pending, 0), nil
}

func (s PluginFeed) advanceStreamForOffer(ctx context.Context, step *streamStep) error {
	if err := s.advanceStream(ctx, step); err != nil {
		// A published reset is an explicit replacement offer, including when
		// the budget expired while preparing a cut rather than at preflight.
		return s.streamResourceFailure(step, err)
	}
	return nil
}

func (s PluginFeed) prepareStreamLaunch(step *streamStep, features streamFeatures) error {
	state := &step.state
	if !step.exists || !streamBindingMatches(*state, step.owner, features) {
		*state = newPluginStreamState(step.owner, features, s.stream.run)
		return state.queueGap("enabled", streamAt(step.head))
	}
	if state.Run == s.stream.run {
		return nil
	}
	state.Run = s.stream.run
	if state.Backoff > 0 {
		s.stream.retryAt = s.stream.now().Add(time.Duration(max(state.Backoff, 1)) * time.Second)
	}
	if state.Pending != nil && state.Pending.Attempted {
		state.ResumeCut = true
		return nil
	}
	state.Pending = nil
	return state.queueGap("launch", streamAt(step.head))
}

func (s PluginFeed) advanceStream(ctx context.Context, step *streamStep) error {
	state := &step.state
	if state.Pending != nil && state.Pending.Kind == "gap" {
		return nil
	}
	position := state.Cursor
	// A single durably attempted receipt is the bounded W continuation,
	// even if later native history was lost. Original guards still gate every
	// part and ACK below. Once it commits, the next reset reports the new cut.
	if reason := streamLost(position, step.head); reason != "" && (state.Pending == nil || !state.Pending.Attempted) {
		return state.queueGap(reason, streamAt(step.head))
	}
	if state.Pending != nil {
		return nil // Inspection preserves the original durable attempt evidence.
	}
	if state.Pending == nil && state.NeedCut {
		s.captureStreamCut(state, step.head)
		return nil
	}
	return s.scanStream(ctx, step)
}

func (s PluginFeed) attemptStreamPart(ctx context.Context, step *streamStep, part int) error {
	pending := step.state.Pending
	if part > len(pending.Frames) {
		return model.NewError(model.CodeInvalidInput, "invalid plugin feed part")
	}
	// A previous rename may be visible without confirmed directory durability.
	// Confirm that namespace before changing the durable attempt evidence.
	if err := fsutil.SyncDir(s.pluginDir()); err != nil {
		return err
	}
	current, err := s.pendingStreamCurrent(ctx, step.view, step.owner, pending, step.budget)
	if err != nil {
		return s.streamResourceFailure(step, err)
	}
	if !current {
		if err := step.state.queueGap("policy", streamAt(step.head)); err != nil {
			return err
		}
		return s.saveStreamState(&step.state)
	}
	if s.stream.offer != pending.ID || part == 1 {
		s.stream.offer, s.stream.nextPart = pending.ID, 1
	}
	if part != s.stream.nextPart {
		return model.NewError(model.CodeConflict, "plugin feed parts must be handed off in order")
	}
	pending.Attempted = true
	if part == len(pending.Frames) {
		pending.EndAttempted = true
	}
	// Atomic publication (including sync) precedes any bytes. An error returns
	// no payload, even when rename may already have published the attempt.
	if err := s.saveStreamState(&step.state); err != nil {
		return err
	}
	s.stream.nextPart++
	return nil // WriteAtomic already confirmed this attempt's publication.
}

func (s PluginFeed) streamResourceFailure(step *streamStep, cause error) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	if errors.Is(cause, errStreamWork) && s.stream.retryWork {
		return cause // another try follows; nothing is reset or published
	}
	s.stream.backoff = min(max(s.stream.backoff*2, 1), 60)
	s.stream.retryAt = s.stream.now().Add(time.Duration(s.stream.backoff) * time.Second)
	step.state.Backoff = s.stream.backoff
	// A refused step publishes only the reset. Earlier filtered records in
	// this same step must not advance the last acknowledged cursor.
	step.state.Cursor = step.start
	reason := "unavailable"
	if errors.Is(cause, errStreamWork) {
		reason = "resource"
	}
	s.stream.waitingPending = &streamWaiting{reason: reason, delay: s.stream.backoff}
	if err := step.state.queueGap(reason, streamAt(step.head)); err != nil {
		return err
	}
	return s.saveStreamState(&step.state)
}
