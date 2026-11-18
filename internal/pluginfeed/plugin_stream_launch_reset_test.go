package pluginfeed

import (
	"bytes"
	"testing"
)

func TestStreamSecondLaunchResetsPrivateView(t *testing.T) {
	feed, oldLaunch, _, _, _ := streamPendingMessageFixture(t)
	old := readStreamCheckpoint(t, feed)
	owner := readCommitFile(t, feed.pluginPath())
	second := NewPluginFeed(feed.store, feed.name)
	captured := streamTestLaunch(t, second)
	gap := streamInspect(t, second, captured)
	if gap.ID == "" || gap.Gap != "launch" || gap.Event != nil {
		t.Fatal("F_SECOND_LAUNCH_RESETS_CHECKPOINT_WITH_EXPLICIT_GAP")
	}
	current := readStreamCheckpoint(t, second)
	if current.Run == old.Run || current.Cursor != old.Cursor || current.LastAck != old.LastAck || len(current.Rows) != 0 || len(current.States) != 0 || len(current.Seen) != 0 {
		t.Fatal("F_SECOND_LAUNCH_CLEARS_PROOF_WITHOUT_ACK_PROGRESS")
	}
	if oldLaunch.Revision != captured.Revision || oldLaunch.Epoch != captured.Epoch || !bytes.Equal(owner, readCommitFile(t, feed.pluginPath())) {
		t.Fatal("F_SECOND_LAUNCH_NEVER_REBINDS_OWNER")
	}
}
