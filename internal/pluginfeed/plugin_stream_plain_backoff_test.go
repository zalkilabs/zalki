package pluginfeed

import (
	"context"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamPlainSuccessfulRecoveryResetsBackoff(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	updateStreamState(t, store, session.ID, model.StateBusy)
	ordinaryBudget := feed.stream.budget
	feed.stream.budget = func(ctx context.Context) *streamBudget {
		budget := ordinaryBudget(ctx)
		budget.used[2] = 512 // The next real native read exhausts the shared budget.
		return budget
	}
	gap := streamInspect(t, feed, launch)
	if gap.Gap != "resource" || gap.ID == "" || readStreamCheckpoint(t, feed).Backoff != 1 || feed.stream.backoff != 1 {
		t.Fatal("B_PLAIN_RESOURCE_EPISODE_IS_REAL_GAP")
	}
	streamComplete(t, feed, launch, gap)
	feed.stream.budget = ordinaryBudget
	recoveredAt := feed.stream.now().Add(2 * time.Second)
	feed.stream.now = func() time.Time { return recoveredAt }
	if cut := streamInspect(t, feed, launch); cut.ID != "" {
		t.Fatal("B_PLAIN_RECOVERY_PRIVATE_CUT")
	}
	if feed.stream.backoff != 0 || readStreamCheckpoint(t, feed).Backoff != 0 {
		t.Fatal("B_PLAIN_SUCCESSFUL_RECOVERY_RESETS_BACKOFF")
	}
}
