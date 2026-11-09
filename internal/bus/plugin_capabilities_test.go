package bus

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func capabilityTestTargets(count int) []string {
	result := make([]string, count)
	for i := range result {
		result[i] = fmt.Sprintf("codex:00000000-0000-4000-8000-%012d", i+1)
	}
	return result
}

func TestPluginCapabilitiesRejectMalformedRecords(t *testing.T) {
	f, _, session := externalPinFixture(t)
	if err := f.GrantPluginCapabilities(PluginCapabilityGrants{Read: []string{session}}); err != nil {
		t.Fatal(err)
	}
	p, _, err := f.loadPlugin()
	if err != nil {
		t.Fatal(err)
	}
	good := p.Capabilities.clone()
	cases := []struct {
		name   string
		mutate func(*PluginCapabilities)
	}{
		{"unknown-version", func(c *PluginCapabilities) { c.Version = 2 }},
		{"invalid-installation", func(c *PluginCapabilities) { c.Installation = "not-an-id" }},
		{"invalid-pin", func(c *PluginCapabilities) { c.PinSHA256 = strings.Repeat("A", 64) }},
		{"missing-revision", func(c *PluginCapabilities) { c.Revision = "" }},
		{"duplicate-target", func(c *PluginCapabilities) { c.Read = []string{session, session} }},
		{"noncanonical-target", func(c *PluginCapabilities) { c.Send = []string{"display-name"} }},
		{"plugin-target", func(c *PluginCapabilities) { c.Send = []string{"plugin:00000000-0000-4000-8000-000000000001"} }},
		{"over-limit-union", func(c *PluginCapabilities) { ids := capabilityTestTargets(17); c.Read = ids[:16]; c.Send = ids[16:] }},
		{"empty-section", func(c *PluginCapabilities) { c.PluginCapabilityGrants = PluginCapabilityGrants{} }},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			p.Capabilities = good.clone()
			row.mutate(p.Capabilities)
			if _, err := f.savePlugin(p); err != nil {
				t.Fatal(err)
			}
			if _, err := f.PluginStatus(); err == nil {
				t.Fatal("accepted malformed capability record", row.name)
			}
		})
	}
}

func TestPluginCapabilityRevocationFencesMetadataReceipt(t *testing.T) {
	f, _, session := externalPinFixture(t)
	if err := f.GrantPluginCapabilities(PluginCapabilityGrants{Read: []string{session}, InboxRead: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	offer, err := f.NextPluginFeed()
	if err != nil || offer.ID == "" {
		t.Fatal(offer, err)
	}
	if err := f.RevokePluginCapabilities(); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Enabled {
		t.Fatal("revoke left installation enabled")
	}
	if status.Capabilities != nil || !slices.Equal(status.Sessions, []string{session}) {
		t.Fatal("revoke changed metadata scope or retained capabilities", status)
	}
	if err := f.AckPluginFeed(offer); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("revoked metadata receipt committed", err)
	}
}

func TestPluginCapabilitiesDefaultDenyAndCallerOwnership(t *testing.T) {
	f, _, session := externalPinFixture(t)
	status, err := f.PluginStatus()
	if err != nil || status.Capabilities != nil {
		t.Fatal("metadata grants became body or send authority", status, err)
	}
	ids := capabilityTestTargets(2)
	input := []string{ids[1], ids[0]}
	if err := f.GrantPluginCapabilities(PluginCapabilityGrants{Read: input}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(input, []string{ids[1], ids[0]}) {
		t.Fatal("grant mutated caller slice")
	}
	status, err = f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.Capabilities.Read[0] = "mutated-return"
	again, err := f.PluginStatus()
	if err != nil || !slices.Equal(again.Capabilities.Read, ids) || !slices.Equal(again.Sessions, []string{session}) {
		t.Fatal("status escaped stored ownership or changed metadata scope", again, err)
	}
	raw, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"capabilities"`) {
		t.Fatal("owner grant was not persisted")
	}
}
