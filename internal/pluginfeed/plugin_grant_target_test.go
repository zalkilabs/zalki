package pluginfeed

import (
	"bytes"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// TestPluginFeedRefusesPluginInboxTargets checks that a feed grant names only
// native sessions. Another plugin's inbox address is refused when the owner
// sets it, and a record that holds one on disk does not load.
func TestPluginFeedRefusesPluginInboxTargets(t *testing.T) {
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	installFeed(t, s)
	feed := NewPluginFeed(s, "fake-notifier")
	other := "plugin:" + model.NewUUID()
	for _, ids := range [][]string{{other}, {a.ID, other}} {
		if err := feed.ConfigurePluginFeed(ids); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("feed grant accepted a plugin inbox address", ids, err)
		}
	}
	if err := feed.ConfigurePluginFeed([]string{a.ID}); err != nil {
		t.Fatal("native feed grant refused", err)
	}
	raw, err := os.ReadFile(feed.pluginPath())
	if err != nil || !bytes.Contains(raw, []byte(a.ID)) {
		t.Fatal("feed record missing", err)
	}
	if err := os.WriteFile(feed.pluginPath(), bytes.ReplaceAll(raw, []byte(a.ID), []byte(other)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := feed.PluginStatus(); err == nil {
		t.Fatal("loaded a feed record that grants a plugin inbox address")
	}
}
