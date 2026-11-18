package pluginfeed

import (
	"bytes"
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

func TestPluginAccessRejectsMalformedRecords(t *testing.T) {
	f, _, session := externalPinFixture(t)
	if err := f.GrantPluginAccess(PluginAccess{Status: true, Read: []string{session}}); err != nil {
		t.Fatal(err)
	}
	p, _, err := f.loadPlugin()
	if err != nil {
		t.Fatal(err)
	}
	good := p
	cases := []struct {
		name   string
		mutate func(*pluginRecord)
	}{
		{"duplicate-target", func(p *pluginRecord) { p.Access.Read = []string{session, session} }},
		{"noncanonical-target", func(p *pluginRecord) { p.Access.Send = []string{"display-name"} }},
		{"plugin-target", func(p *pluginRecord) { p.Access.Send = []string{"plugin:00000000-0000-4000-8000-000000000001"} }},
		{"over-limit-union", func(p *pluginRecord) {
			ids := capabilityTestTargets(17)
			p.Access.Read = ids[:16]
			p.Access.Send = ids[16:]
		}},
		{"enabled-without-access", func(p *pluginRecord) { p.Access = PluginAccess{}; p.Enabled = true; p.Epoch = model.NewUUID() }},
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			p := good
			p.Access = good.Access.clone()
			row.mutate(&p)
			if _, err := f.savePlugin(p); err != nil {
				t.Fatal(err)
			}
			if _, err := f.PluginStatus(); err == nil {
				t.Fatal("accepted malformed access record", row.name)
			}
		})
	}
}

func TestPluginAccessRevocationFencesMetadataReceipt(t *testing.T) {
	f, _, session := externalPinFixture(t)
	if err := f.GrantPluginAccess(PluginAccess{Status: true, Read: []string{session}}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	captured, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	captured.FeedFeatures.Feed = true
	offer, err := nextFeed(f)
	if err != nil || offer.ID == "" {
		t.Fatal(offer, err)
	}
	checkpoint, err := os.ReadFile(f.streamPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.RevokePluginAccess(); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Enabled {
		t.Fatal("revoke left installation enabled")
	}
	if !status.Access.empty() || status.Scope != "all" {
		t.Fatal("revoke changed the scope or kept access", status)
	}
	if _, err := f.AckPluginFeed(&captured, offer.ID); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("revoked metadata receipt committed", err)
	}
	after, err := os.ReadFile(f.streamPath())
	if err != nil || !bytes.Equal(checkpoint, after) {
		t.Fatal("revoked ACK changed checkpoint", err)
	}
}

func TestPluginAccessDefaultDenyAndCallerOwnership(t *testing.T) {
	f, _, _ := externalPinFixture(t)
	status, err := f.PluginStatus()
	if err != nil || len(status.Access.Read)+len(status.Access.Send) != 0 || status.Access.Receive {
		t.Fatal("feed access became body or send authority", status, err)
	}
	ids := capabilityTestTargets(2)
	input := []string{ids[1], ids[0]}
	if err := f.GrantPluginAccess(PluginAccess{Status: true, Read: input}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(input, []string{ids[1], ids[0]}) {
		t.Fatal("grant mutated caller slice")
	}
	status, err = f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.Access.Read[0] = "mutated-return"
	again, err := f.PluginStatus()
	if err != nil || !slices.Equal(again.Access.Read, ids) || again.Scope != "all" {
		t.Fatal("status escaped stored ownership or changed the scope", again, err)
	}
	raw, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"access"`) {
		t.Fatal("owner grant was not persisted")
	}
}
