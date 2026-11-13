package pluginfeed

import (
	"context"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// SECURITY (slice W): a launch without message grants is checked by its
// launch binding alone. These tests prove that this branch still fences
// every owner change, and that such a launch can never reach a body path.

func feedOnlyLaunch(t *testing.T) (bus.Store, PluginFeed, model.Session, PluginFeedStatus) {
	t.Helper()
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	enableFeed(t, s, a.ID)
	f := NewPluginFeed(s, "fake-notifier")
	status, err := f.PluginStatus()
	if err != nil || status.Capabilities != nil || status.Epoch == "" {
		t.Fatal("feed-only launch", status, err)
	}
	return s, f, a, status
}

// grantInPlace gives the record message grants WITHOUT a disable/enable, so
// the epoch stays: only the capability check can see the change.
func grantInPlace(t *testing.T, f PluginFeed, session string) {
	t.Helper()
	p, _, err := f.loadPlugin()
	if err != nil {
		t.Fatal(err)
	}
	p.Capabilities = &PluginCapabilities{Version: 1, Installation: p.ID, PinSHA256: p.Launch.PinSHA256, Revision: model.NewUUID(), PluginCapabilityGrants: PluginCapabilityGrants{Receive: []string{session}}}
	if _, err := f.savePlugin(p); err != nil {
		t.Fatal(err)
	}
}

func TestFeedOnlyLaunchBindingStillFences(t *testing.T) {
	for name, change := range map[string]func(*testing.T, PluginFeed, model.Session, *PluginFeedStatus) string{
		"unchanged": func(t *testing.T, _ PluginFeed, _ model.Session, _ *PluginFeedStatus) string {
			t.Helper()
			return "ok"
		},
		"disabled": func(t *testing.T, f PluginFeed, _ model.Session, _ *PluginFeedStatus) string {
			t.Helper()
			if err := f.EnablePluginFeed(false); err != nil {
				t.Fatal(err)
			}
			return "revoked"
		},
		"new epoch": func(t *testing.T, f PluginFeed, _ model.Session, _ *PluginFeedStatus) string {
			t.Helper()
			if err := f.EnablePluginFeed(false); err != nil {
				t.Fatal(err)
			}
			if err := f.EnablePluginFeed(true); err != nil {
				t.Fatal(err)
			}
			return "stale_binding"
		},
		"other pin": func(t *testing.T, _ PluginFeed, _ model.Session, launch *PluginFeedStatus) string {
			t.Helper()
			pinned := *launch.Launch
			pinned.PinSHA256 = strings.Repeat("a", 64)
			launch.Launch = &pinned
			return "stale_binding"
		},
		"other installation": func(t *testing.T, _ PluginFeed, _ model.Session, launch *PluginFeedStatus) string {
			t.Helper()
			launch.ID = model.NewUUID()
			return "stale_binding"
		},
		"grants appear in the same epoch": func(t *testing.T, f PluginFeed, a model.Session, _ *PluginFeedStatus) string {
			t.Helper()
			grantInPlace(t, f, a.ID)
			return "stale_binding"
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, f, a, launch := feedOnlyLaunch(t)
			want := change(t, f, a, &launch)
			if got := f.PluginBindingCode(&launch); got != want {
				t.Fatalf("binding code %q, want %q", got, want)
			}
		})
	}
}

// Even when the record has grants, a launch that started without them can
// use no body path: inbox_next, inbox_consume, native read, send/reply.
func TestFeedOnlyLaunchCannotReachBodyPaths(t *testing.T) {
	s, f, a, launch := feedOnlyLaunch(t)
	grantInPlace(t, f, a.ID)
	if m, code := f.NextPluginInbox(&launch); m != nil || code == "ok" {
		t.Fatal("inbox_next for a feed-only launch", code)
	}
	if code := f.ConsumePluginInbox(&launch, model.NewUUID()); code == "ok" {
		t.Fatal("inbox_consume for a feed-only launch", code)
	}
	if raw, code, _ := f.ReadPluginMessageBound(&launch, a.ID, model.NewUUID()); code != "denied" || strings.Contains(string(raw), `"body"`) {
		t.Fatal("native read for a feed-only launch", code)
	}
	service := bus.Service{Store: s, PluginSend: BoundPluginSend(&launch)}
	if _, err := service.SendFromPlugin(context.Background(), bus.PluginSendRequest{Installation: launch.ID, Name: launch.Name, ClientMessageID: "c1", To: a.ID, Body: "hello"}); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("send for a feed-only launch", err)
	}
}

// The owner read paths refuse a plugin that has no grants.
func TestOwnerReadsRefuseAPluginWithoutGrants(t *testing.T) {
	_, f, a, _ := feedOnlyLaunch(t)
	if raw, err := f.ReadPluginInboxMessage(model.NewUUID()); raw != nil || err == nil {
		t.Fatal("owner inbox read without a receive grant", err)
	}
	if raw, err := f.ReadPluginMessage(a.ID, model.NewUUID()); raw != nil || err == nil {
		t.Fatal("owner read without a read grant", err)
	}
}

// A launch WITH grants whose revision is stale stays refused.
func TestLaunchWithGrantsAndStaleRevisionIsRefused(t *testing.T) {
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	installFeed(t, s)
	f := NewPluginFeed(s, "fake-notifier")
	if err := f.ConfigurePluginFeed([]string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.GrantPluginCapabilities(PluginCapabilityGrants{Receive: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	launch, err := f.PluginStatus()
	if err != nil || launch.Capabilities == nil {
		t.Fatal(launch, err)
	}
	if code := f.PluginBindingCode(&launch); code != "ok" {
		t.Fatal("current launch refused", code)
	}
	stale := launch
	stale.Capabilities = launch.Capabilities.clone()
	stale.Capabilities.Revision = model.NewUUID()
	if code := f.PluginBindingCode(&stale); code != "stale_binding" {
		t.Fatal("stale revision accepted", code)
	}
	if m, code := f.NextPluginInbox(&stale); m != nil || code == "ok" {
		t.Fatal("inbox_next with a stale revision", code)
	}
	// Same revision, but the grants no longer bind to this pin.
	p, _, err := f.loadPlugin()
	if err != nil {
		t.Fatal(err)
	}
	p.Capabilities.PinSHA256 = strings.Repeat("a", 64)
	if _, err := f.savePlugin(p); err != nil {
		t.Fatal(err)
	}
	if code := f.PluginBindingCode(&launch); code != "stale_binding" {
		t.Fatal("grants bound to another pin accepted", code)
	}
}
