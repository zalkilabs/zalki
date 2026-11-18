package pluginfeed

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

type recordedStreamScan struct {
	budget         *streamBudget
	events         int
	firstForbidden int
	ids            map[string]bool
}

func recordStreamScanWork(t *testing.T, feed PluginFeed) *[]*recordedStreamScan {
	t.Helper()
	steps := []*recordedStreamScan{}
	makeBudget := feed.stream.budget
	feed.stream.budget = func(ctx context.Context) *streamBudget {
		budget := makeBudget(ctx) // Preserve the fixture's frozen work clock.
		step := &recordedStreamScan{budget: budget, ids: map[string]bool{}}
		budget.eventRead = func(view bus.JournalView, epoch string, sequence uint64) (model.Event, error) {
			// Only scanStream calls this dependency, from withStreamStep's
			// acquired journal view. A checkpoint inspection may allocate a
			// budget too, but cannot claim a scan record without EventAt.
			if step.events == 0 {
				steps = append(steps, step)
			}
			step.events++
			// Record attempted operation65 before the real dependency call.
			// This observer never aborts, resets or imposes another resource
			// cap; the root asserts this witness before its cursor checks.
			if step.events == 65 {
				step.firstForbidden = step.events
			}
			event, err := view.EventAt(epoch, sequence)
			if err == nil {
				if event.Message != nil || step.ids[event.Session.ID] {
					t.Fatal("scan fixture no longer has distinct session facts")
				}
				step.ids[event.Session.ID] = true
			}
			return event, err
		}
		return budget
	}
	return &steps
}

func addDeniedStreamScanFacts(t *testing.T, store bus.Store, count int) {
	t.Helper()
	for range count {
		session, _ := register(t, store, "codex")
		path, err := sessionTestPath(store, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		session.Owner = &model.ProcessRef{PID: 999999, Start: "gone"}
		if err := writeJSON(path, session); err != nil {
			t.Fatal(err)
		}
	}
}

func assertStreamScanWork(t *testing.T, recorded *[]*recordedStreamScan, index, want int) {
	t.Helper()
	if len(*recorded) != index+1 {
		t.Fatal("scan did not use exactly one new acquired step", len(*recorded), index)
	}
	step := (*recorded)[index]
	// Distinct, nonretired, valid records make each real EventAt feed exactly
	// one target to collectPluginMemberProjections/projectPluginMember, whose
	// authoritative PluginMember call causes one uncached readSessionEvidence
	// debit and one sessionLiveness probe debit. The current source placements
	// and these fixture conditions are the1:1 link; these are not generic debit
	// counts for arbitrary targets/cache hits or a second membership model.
	if step.events != want || len(step.ids) != want || step.budget.used[2] != want || step.budget.used[4] != want {
		t.Fatal("actual event/member/probe operation counts differ", step.events, len(step.ids), step.budget.used)
	}
	if step.budget.used[0] >= 4<<20 || step.budget.used[1] >= 640 || step.budget.used[3] >= 262144 {
		t.Fatal("another resource cap masked the per-step operation bound", step.budget.used)
	}
}

func TestStreamDeniedBacklogAdvances64Then64Then1(t *testing.T) {
	store := fixtureStore(t)
	enableFeed(t, store)
	feed := fixtureFeedRun(NewPluginFeed(store, "fake-notifier"))
	launch := streamTestLaunch(t, feed)
	if cut := streamInspect(t, feed, launch); cut.ID != "" {
		t.Fatal("initial acknowledged cut emitted a frame")
	}
	start := readStreamCheckpoint(t, feed).Cursor
	addDeniedStreamScanFacts(t, store, 129)
	assertDeniedStreamFixtureFitsByteCap(t, store, start.Sequence)
	measured := recordStreamScanWork(t, feed)
	// Exercise the wrapped factory outside a scan before any real step. This
	// inspection must not enter the acquired-scan ledger or shift its indices.
	_ = readStreamCheckpoint(t, feed)
	if len(*measured) != 0 {
		t.Fatal("checkpoint inspection claimed an acquired scanner step")
	}
	sequence := start.Sequence
	for index, want := range []int{64, 64, 1} {
		delivery, err := feed.NextPluginFeed(t.Context(), launch, "", 0)
		if len(*measured) > index && (*measured)[index].firstForbidden != 0 {
			t.Fatal("F_FEED_LOCKED_STEP_AT_MOST_64_RECORDS", (*measured)[index].firstForbidden)
		}
		if err != nil || delivery.ID != "" || len(delivery.Frames) != 0 {
			t.Fatal("denied backlog was offered or reset", index, delivery, err)
		}
		sequence += uint64(want)
		current := readStreamCheckpoint(t, feed)
		if current.Cursor.Sequence != sequence || current.Cursor.Epoch != eventState(t, store).Epoch || current.Pending != nil || current.NeedCut || current.Backoff != 0 || !feed.stream.retryAt.IsZero() {
			t.Fatal("denied scan cursor/reset invariant", index, current.Cursor, sequence)
		}
		assertStreamScanWork(t, measured, index, want)
	}
}

func assertDeniedStreamFixtureFitsByteCap(t *testing.T, store bus.Store, startSequence uint64) {
	t.Helper()
	// Measure the actual129 metadata encodings. Even the whole backlog fits
	// the scanner's byte cap, so it cannot hide removing scanned<64.
	bytes := 0
	inView(t, store, func(view bus.JournalView) {
		head, err := view.EventWindow()
		if err != nil || head.Last != startSequence+129 {
			t.Fatal("fixture added other post-cut facts", err, head)
		}
		for sequence := startSequence + 1; sequence <= head.Last; sequence++ {
			event, err := view.EventAt(head.Epoch, sequence)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			bytes += len(raw)
		}
	})
	if bytes >= streamBatchLimit {
		t.Fatal("fixture byte cap could mask removed operation limit", bytes)
	}
}
