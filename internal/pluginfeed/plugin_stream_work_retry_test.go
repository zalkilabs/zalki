package pluginfeed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// missingBudget returns a budget factory whose first `misses` budgets are
// already used up for native reads and counts every budget it hands out.
func missingBudget(feed PluginFeed, misses int, made *int) func(context.Context) *streamBudget {
	ordinary := feed.stream.budget
	return func(ctx context.Context) *streamBudget {
		budget := ordinary(ctx)
		*made++
		if *made <= misses {
			budget.used[2] = 512 // the next real native read misses the budget
		}
		return budget
	}
}

func TestStreamWorkMissRetriesWithoutResettingTheStream(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	before := readStreamCheckpoint(t, feed)
	if before.States[session.ID].State != model.StateReady && before.States[session.ID].State != model.StateBusy {
		t.Fatal("W_RETRY_FIXTURE_HAS_NO_SEEN_STATE")
	}
	updateStreamState(t, store, session.ID, model.StateBusy)
	var made int
	feed.stream.budget = missingBudget(feed, 1, &made)
	offer, frame := streamTake(t, feed, launch)
	if frame.Gap != "" || frame.Event == nil || frame.Event.Session.State != model.StateBusy || offer.Gap != "" {
		t.Fatal("W_RETRY_ONE_MISS_MUST_NOT_QUEUE_A_GAP", frame.Gap)
	}
	after := readStreamCheckpoint(t, feed)
	if made < 2 || feed.stream.backoff != 0 || after.Backoff != 0 || after.LastGap == "resource" || len(after.States) == 0 || len(after.Rows) == 0 {
		t.Fatal("W_RETRY_ONE_MISS_KEEPS_EVIDENCE", made, after.LastGap, after.Backoff)
	}
}

func TestStreamWorkMissesStillQueueAResourceGapAfterTheLastTry(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	updateStreamState(t, store, session.ID, model.StateBusy)
	var made int
	feed.stream.budget = missingBudget(feed, streamWorkAttempts, &made)
	gap := streamInspect(t, feed, launch)
	if gap.Gap != "resource" || gap.ID == "" || made != streamWorkAttempts {
		t.Fatal("W_RETRY_LAST_MISS_IS_A_RESOURCE_GAP", gap.Gap, made)
	}
	state := readStreamCheckpoint(t, feed)
	if state.Backoff != 1 || feed.stream.backoff != 1 || len(state.States) != 0 {
		t.Fatal("W_RETRY_LAST_MISS_KEEPS_TODAYS_RESET", state.Backoff, len(state.States))
	}
	if feed.stream.retryWork {
		t.Fatal("W_RETRY_FLAG_LEAKED")
	}
}

func TestStreamAckWorkMissRetriesAndCommits(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	updateStreamState(t, store, session.ID, model.StateBusy)
	offer, _ := streamTake(t, feed, launch)
	var made int
	feed.stream.budget = missingBudget(feed, 1, &made)
	if _, err := feed.AckPluginFeed(launch, offer.ID); err != nil {
		t.Fatal("W_RETRY_ACK_ONE_MISS_IS_NOT_TERMINAL", err)
	}
	state := readStreamCheckpoint(t, feed)
	if made < 2 || state.Pending != nil || state.LastAck != offer.ID || state.States[session.ID].State != model.StateBusy {
		t.Fatal("W_RETRY_ACK_COMMITS_AFTER_A_MISS", made, state.LastAck)
	}
}

func TestStreamAckWorkMissesAreStillAnErrorAfterTheLastTry(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	updateStreamState(t, store, session.ID, model.StateBusy)
	offer, _ := streamTake(t, feed, launch)
	var made int
	feed.stream.budget = missingBudget(feed, streamWorkAttempts, &made)
	if _, err := feed.AckPluginFeed(launch, offer.ID); err == nil || made != streamWorkAttempts {
		t.Fatal("W_RETRY_ACK_LAST_MISS_STAYS_AN_ERROR", err, made)
	}
	if feed.stream.retryWork {
		t.Fatal("W_RETRY_FLAG_LEAKED")
	}
}

// streamMemory is the part of the in-memory stream runtime an ACK must leave
// alone when it only ran out of work allowance. The budget and clock functions
// cannot be compared and are not part of it.
type streamMemory struct {
	retryAt          time.Time
	backoff          int
	offer            string
	nextPart         int
	controlID        string
	integrityEpisode bool
	integrityPending *streamIntegrity
	waitingPending   *streamWaiting
	retryWork        bool
}

func memoryOf(runtime *streamRuntime) streamMemory {
	return streamMemory{runtime.retryAt, runtime.backoff, runtime.offer, runtime.nextPart, runtime.controlID,
		runtime.integrityEpisode, runtime.integrityPending, runtime.waitingPending, runtime.retryWork}
}

func TestStreamAckExhaustedWorkKeepsTheOfferForALaterAck(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	updateStreamState(t, store, session.ID, model.StateBusy)
	offer, _ := streamTake(t, feed, launch)
	ordinary := feed.stream.budget
	memory := memoryOf(feed.stream)
	var made int
	feed.stream.budget = missingBudget(feed, streamWorkAttempts, &made)
	if _, err := feed.AckPluginFeed(launch, offer.ID); !errors.Is(err, ErrFeedWork) || made != streamWorkAttempts {
		t.Fatal("W_ACK_EXHAUSTED_WORK_IS_THE_PUBLIC_SIGNAL", err, made)
	}
	if memoryOf(feed.stream) != memory {
		t.Fatal("W_ACK_EXHAUSTED_WORK_LEAVES_THE_RUNTIME_ALONE", memoryOf(feed.stream), memory)
	}
	state := readStreamCheckpoint(t, feed)
	if state.Pending == nil || state.Pending.ID != offer.ID || state.LastAck == offer.ID || state.Backoff != 0 {
		t.Fatal("W_ACK_EXHAUSTED_WORK_KEEPS_THE_PENDING_OFFER")
	}
	feed.stream.budget = ordinary
	if _, err := feed.AckPluginFeed(launch, offer.ID); err != nil {
		t.Fatal("W_ACK_LATER_CALL_COMMITS", err)
	}
	state = readStreamCheckpoint(t, feed)
	if state.Pending != nil || state.LastAck != offer.ID || state.States[session.ID].State != model.StateBusy {
		t.Fatal("W_ACK_LATER_CALL_COMMITS_THE_EVIDENCE", state.LastAck)
	}
}
