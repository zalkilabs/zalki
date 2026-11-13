package pluginfeed

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInstallEnabledRecordHasEnableIncarnation(t *testing.T) {
	feed := installForTest(t, fixtureStore(t))
	installed := feedRecordNamed(t, feed)
	if !installed.Enabled || !model.IsCanonicalUUID(installed.Epoch) {
		t.Fatal("F_S4_ENABLED_INSTALL_SEEDS_EPOCH")
	}
	launch, err := feed.PluginStatus()
	if err != nil || launch.Epoch != installed.Epoch || feed.PluginBindingCode(&launch) != "ok" {
		t.Fatal("F_S4_INSTALLED_ENABLE_INCARNATION_IS_RUNNABLE", err)
	}
}
