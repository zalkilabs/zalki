package pluginfeed

import (
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamActualInstallSeedsEnableIncarnation(t *testing.T) {
	store := fixtureStore(t)
	register(t, store, "codex")
	feed := installForTest(t, store) // real BeginPluginInstall/Commit/commitInstall
	var installed pluginRecord
	if err := json.Unmarshal(readCommitFile(t, feed.pluginPath()), &installed); err != nil {
		t.Fatal(err)
	}
	if !model.IsCanonicalUUID(installed.Epoch) {
		t.Fatal("F_S4_ENABLED_INSTALL_SEEDS_EPOCH")
	}
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	state, exists, err := feed.loadStreamState(newStreamBudget(t.Context()))
	head := eventState(t, store)
	if err != nil || !exists || state.Enable != launch.Epoch || len(state.Rows) != 0 || state.NeedCut || state.Cursor != (pluginPosition{Epoch: head.Epoch, Sequence: head.Last}) {
		t.Fatal("F_S4_ACTUAL_INSTALL_INITIALIZES_FEED_CHECKPOINT", err)
	}
}
