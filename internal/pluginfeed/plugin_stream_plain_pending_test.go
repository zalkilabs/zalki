package pluginfeed

import (
	"bytes"
	"testing"
)

func TestStreamPlainInspectionPreservesAttemptedReceipt(t *testing.T) {
	feed, launch, _, _, offer := streamPendingMessageFixture(t)
	streamAttemptAll(t, feed, launch, offer)
	before := checkpointImage(t, feed)
	for range 3 {
		if again := streamInspect(t, feed, launch); again.ID != offer.ID {
			t.Fatal("F_PLAIN_INSPECT_SAME_PENDING_ID")
		}
		pending := readStreamCheckpoint(t, feed).Pending
		if pending == nil || !pending.Attempted || !pending.EndAttempted || !bytes.Equal(before, checkpointImage(t, feed)) {
			t.Fatal("F_PLAIN_INSPECT_NEVER_DISCARDS_SAVED_ATTEMPT")
		}
	}
	if _, err := feed.AckPluginFeed(launch, offer.ID); err != nil {
		t.Fatal("F_PLAIN_SAVED_ATTEMPT_ACK_WITHOUT_REHANDOFF", err)
	}
}
