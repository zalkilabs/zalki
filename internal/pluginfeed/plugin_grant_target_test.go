package pluginfeed

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

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

func TestNextPluginFeedIsBoundToTheLaunch(t *testing.T) {
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	b, _ := register(t, s, "codex")
	enableFeed(t, s, a.ID)
	feed := NewPluginFeed(s, "fake-notifier")
	launch, err := feed.PluginStatus()
	if err != nil || launch.Epoch == "" {
		t.Fatal("enable gave no epoch", launch, err)
	}
	for _, sessions := range [][]string{{b.ID}, nil} {
		checkOldLaunchRefused(t, feed, launch, sessions)
	}
}

// checkOldLaunchRefused disables, sets sessions (if any) and enables again,
// then checks that the old launch gets no offer and a new one does.
func checkOldLaunchRefused(t *testing.T, feed PluginFeed, launch PluginFeedStatus, sessions []string) {
	t.Helper()
	if err := feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if sessions != nil {
		if err := feed.ConfigurePluginFeed(sessions); err != nil {
			t.Fatal(err)
		}
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if d, err := feed.NextPluginFeed(&launch); model.ErrorCodeOf(err) != model.CodeUnauthorized || d.ID != "" {
		t.Fatal("an old launch got a feed offer", sessions, d, err)
	}
	current, err := feed.PluginStatus()
	if err != nil || current.Epoch == launch.Epoch {
		t.Fatal("enable did not rotate the epoch", err)
	}
	if d, err := feed.NextPluginFeed(&current); err != nil || d.Gap != "enabled" {
		t.Fatal("the new launch got no feed", d, err)
	}
}

func TestLaunchBindingCodeComparesTheWholeLaunch(t *testing.T) {
	pin := &PluginLaunch{PinSHA256: strings.Repeat("a", 64)}
	caps := &PluginCapabilities{Revision: "r1"}
	launch := &PluginFeedStatus{ID: "p", Enabled: true, Epoch: "e1", Sessions: []string{"codex:a"}, Launch: pin, Capabilities: caps}
	record := func() pluginRecord {
		return pluginRecord{ID: "p", Enabled: true, Epoch: "e1", Sessions: []string{"codex:a"}, Launch: &PluginLaunch{PinSHA256: pin.PinSHA256}, Capabilities: &PluginCapabilities{Revision: "r1"}}
	}
	if code := launchBindingCode(record(), launch); code != "ok" {
		t.Fatal("same launch", code)
	}
	for name, change := range map[string]func(*pluginRecord){
		"id":       func(p *pluginRecord) { p.ID = "q" },
		"epoch":    func(p *pluginRecord) { p.Epoch = "e2" },
		"sessions": func(p *pluginRecord) { p.Sessions = []string{"codex:b"} },
		"pin":      func(p *pluginRecord) { p.Launch.PinSHA256 = strings.Repeat("b", 64) },
		"revision": func(p *pluginRecord) { p.Capabilities.Revision = "r2" },
		"no caps":  func(p *pluginRecord) { p.Capabilities = nil },
	} {
		p := record()
		change(&p)
		if code := launchBindingCode(p, launch); code != "stale_binding" {
			t.Error(name, "changed but the launch still holds:", code)
		}
	}
	p := record()
	p.Enabled = false
	if code := launchBindingCode(p, launch); code != "revoked" {
		t.Error("disabled:", code)
	}
	if code := launchBindingCode(record(), nil); code != "stale_binding" {
		t.Error("no launch:", code)
	}
}

// TestNativeReadIsBoundToTheLaunch reads a body with a valid launch, then
// disables and enables the plugin with the same grants. The old launch must
// get stale_binding and no body.
func TestNativeReadIsBoundToTheLaunch(t *testing.T) {
	x := newPluginReadFixture(t, "cursor")
	if _, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now); err != nil {
		t.Fatal("control read", err)
	}
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	raw, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now.Add(2*time.Second))
	if len(raw) != 0 || err == nil || !strings.Contains(err.Error(), "stale_binding") || strings.Contains(err.Error(), x.message.Body) {
		t.Fatal("an old launch read a body after a re-enable", len(raw), err)
	}
}
