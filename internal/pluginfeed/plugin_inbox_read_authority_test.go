package pluginfeed

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxReadAuthorityAndSnapshotFences(t *testing.T) {
	for _, kind := range []string{"disabled", "owner-disable", "owner-revoke", "no-capabilities", "read-only", "send-only", "inbox-from-only", "foreign-installation", "foreign-pin", "changed-launch-pin", "replaced-installation", "checked-installation", "checked-pin", "checked-revision"} {
		t.Run(kind, func(t *testing.T) { checkInboxReadAuthority(t, kind) })
	}
}
func checkInboxReadAuthority(t *testing.T, kind string) {
	t.Helper()
	x := newInboxReadFixture(t, "")
	before := x.status
	if kind == "read-only" || kind == "send-only" || kind == "inbox-from-only" {
		if err := x.f.EnablePluginFeed(false); err != nil {
			t.Fatal(err)
		}
		g := PluginCapabilityGrants{InboxFrom: []string{x.from.ID}}
		if kind == "read-only" {
			g.Read = []string{x.from.ID}
		}
		if kind == "send-only" {
			g.Send = []string{x.from.ID}
		}
		if err := x.f.GrantPluginCapabilities(g); err != nil {
			t.Fatal(err)
		}
		if err := x.f.EnablePluginFeed(true); err != nil {
			t.Fatal(err)
		}
		raw, err := x.f.ReadPluginInboxMessage(x.message.ID)
		assertInboxReadDenied(t, raw, err, "denied")
		return
	}
	if strings.HasPrefix(kind, "checked-") {
		before = alteredInboxReadSnapshot(before, kind)
	} else {
		tamperInboxReadAuthority(t, x, kind)
	}
	raw, err := x.read(t, &before, x.now)
	assertInboxReadDenied(t, raw, err, "")
}
func alteredInboxReadSnapshot(before PluginFeedStatus, kind string) PluginFeedStatus {
	before.Capabilities = before.Capabilities.clone()
	launch := *before.Launch
	before.Launch = &launch
	switch kind {
	case "checked-installation":
		before.ID = model.NewUUID()
	case "checked-pin":
		before.Launch.PinSHA256 = strings.Repeat("a", 64)
	case "checked-revision":
		before.Capabilities.Revision = model.NewUUID()
	}
	return before
}
func tamperInboxReadAuthority(t *testing.T, x inboxReadFixture, kind string) {
	t.Helper()
	p, _, err := x.f.loadPlugin()
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "owner-disable":
		if err = x.f.EnablePluginFeed(false); err != nil {
			t.Fatal(err)
		}
		return
	case "owner-revoke":
		if err = x.f.RevokePluginCapabilities(); err != nil {
			t.Fatal(err)
		}
		return
	case "disabled":
		p.Enabled = false
	case "no-capabilities":
		p.Capabilities = nil
	case "foreign-installation":
		p.Capabilities.Installation = model.NewUUID()
	case "foreign-pin":
		p.Capabilities.PinSHA256 = strings.Repeat("a", 64)
	case "changed-launch-pin":
		p.Launch.PinSHA256 = strings.Repeat("a", 64)
	case "replaced-installation":
		p.ID = model.NewUUID()
		p.Capabilities.Installation = p.ID
	}
	if _, err = x.f.savePlugin(p); err != nil {
		t.Fatal(err)
	}
}

func TestPluginInboxReadRequiresCurrentExecutableAndExactSelectors(t *testing.T) {
	x := newInboxReadFixture(t, "")
	for _, id := range []string{"", "../message", strings.ToUpper("abcdef00-0000-4000-8000-000000000001"), "cursor:" + x.message.ID} {
		raw, err := x.f.ReadPluginInboxMessage(id)
		assertInboxReadDenied(t, raw, err, "invalid_input")
	}
	raw, err := NewPluginFeed(x.f.store, "../outside").ReadPluginInboxMessage(x.message.ID)
	assertInboxReadDenied(t, raw, err, "invalid_input")
	if err = os.WriteFile(x.status.Launch.Executable, []byte("#!/bin/sh\nexit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err = x.f.ReadPluginInboxMessage(x.message.ID)
	assertInboxReadDenied(t, raw, err, "stale_binding")
}
func TestPluginInboxReadRegrantAndReinstallDoNotRefreshOldSnapshot(t *testing.T) {
	x := newInboxReadFixture(t, "")
	old := x.status
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	// The sender stays in accept_from: only a sender the plugin may still
	// hear from is offered (see TestInboxOffersOnlyCurrentSenders).
	if err := x.f.GrantPluginCapabilities(PluginCapabilityGrants{InboxRead: true, InboxFrom: []string{x.from.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	raw, err := x.read(t, &old, x.now)
	assertInboxReadDenied(t, raw, err, "stale_binding")
	// Current read authority may read a prior admission in the SAME installation.
	current, err := x.f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	raw, err = x.read(t, &current, x.now.Add(2*time.Second))
	x.checkResult(t, raw, err)
	if err = x.f.RevokePluginCapabilities(); err != nil {
		t.Fatal(err)
	}
	if err = x.f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if err = x.f.AddExternalPlugin(old.Launch.Executable, old.Launch.Args); err != nil {
		t.Fatal(err)
	}
	if err = x.f.ConfigurePluginFeed(old.Sessions); err != nil {
		t.Fatal(err)
	}
	if err = x.f.GrantPluginCapabilities(PluginCapabilityGrants{InboxRead: true}); err != nil {
		t.Fatal(err)
	}
	if err = x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	raw, err = x.read(t, &current, x.now.Add(4*time.Second))
	assertInboxReadDenied(t, raw, err, "stale_binding")
	raw, err = x.f.ReadPluginInboxMessage(x.message.ID)
	assertInboxReadDenied(t, raw, err, "denied")
}
