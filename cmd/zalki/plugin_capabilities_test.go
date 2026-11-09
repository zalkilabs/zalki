package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

type capabilityOwnerStatus struct {
	bus.PluginFeedStatus
	CapabilitiesBound *bool `json:"capabilities_bound"`
	PinMatches        *bool `json:"pin_matches"`
}

func capabilityOwnerFixture(t *testing.T) (string, bus.Store, string, string) {
	t.Helper()
	binary := checkRealBinary(t)
	store, session := pluginOwnerFixture(t)
	executable := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	capabilityCLIOK(t, binary, store.Dir, "add", "external", "--exec", executable)
	capabilityCLIOK(t, binary, store.Dir, "configure", "external", "--session", session.ID)
	return binary, store, executable, session.ID
}

func capabilityCLIOK(t *testing.T, binary, state string, args ...string) {
	t.Helper()
	if raw, err := pluginOwnerCLI(t, binary, state, args...); err != nil {
		t.Fatal(args, string(raw), err)
	}
}

func readCapabilityOwnerStatus(t *testing.T, binary, state string) capabilityOwnerStatus {
	t.Helper()
	raw, err := pluginOwnerCLI(t, binary, state, "status", "external")
	var status capabilityOwnerStatus
	if err != nil || json.Unmarshal(raw, &status) != nil {
		t.Fatal("capability status failed", string(raw), err)
	}
	return status
}

func TestPluginCapabilityOwnerCLIReplacementStatusAndRevoke(t *testing.T) {
	binary, store, _, session := capabilityOwnerFixture(t)
	initial := readCapabilityOwnerStatus(t, binary, store.Dir)
	if initial.Capabilities != nil {
		t.Fatal("metadata scope implicitly granted body or message permissions")
	}
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--read", session, "--send", session, "--accept-from", session, "--inbox-read")
	first := readCapabilityOwnerStatus(t, binary, store.Dir)
	c := assertCapabilityOwnerFullGrant(t, &first, session)
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--inbox-read")
	second := readCapabilityOwnerStatus(t, binary, store.Dir)
	if second.Capabilities == nil || !second.Capabilities.InboxRead || len(second.Capabilities.Read)+len(second.Capabilities.Send)+len(second.Capabilities.InboxFrom) != 0 {
		t.Fatal("grant did not replace omitted permissions", second)
	}
	if second.Capabilities.Revision == c.Revision {
		t.Fatal("grant reused previous revision")
	}
	capabilityCLIOK(t, binary, store.Dir, "enable", "external")
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, "grant", "external", "--read", session); err == nil {
		t.Fatal("granted while enabled", string(raw))
	}
	capabilityCLIOK(t, binary, store.Dir, "revoke", "external")
	final := readCapabilityOwnerStatus(t, binary, store.Dir)
	if final.Enabled {
		t.Fatal("revoke left installation enabled")
	}
	if final.Capabilities != nil || !slices.Equal(final.Sessions, initial.Sessions) {
		t.Fatal("revoke retained permissions or changed metadata scope", final)
	}
}

func TestPluginCapabilityOwnerCLIMetadataConfigurePreservesGrants(t *testing.T) {
	binary, store, _, session := capabilityOwnerFixture(t)
	before := readCapabilityOwnerStatus(t, binary, store.Dir)
	if before.Capabilities != nil {
		t.Fatal("metadata scope implicitly granted body or message permissions")
	}
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--read", session, "--inbox-read")
	granted := readCapabilityOwnerStatus(t, binary, store.Dir)
	const other = "codex:00000000-0000-4000-8000-000000000002"
	capabilityCLIOK(t, binary, store.Dir, "configure", "external", "--session", other)
	after := readCapabilityOwnerStatus(t, binary, store.Dir)
	if !reflect.DeepEqual(granted.Capabilities, after.Capabilities) {
		t.Fatal("metadata configure discarded capability grants", after)
	}
	if !slices.Equal(after.Sessions, []string{other}) || !slices.Equal(after.Capabilities.Read, []string{session}) || len(after.Capabilities.Send) != 0 {
		t.Fatal("metadata scope changed body or send authority", after)
	}
}

func TestPluginCapabilityOwnerCLIPinAndReaddFence(t *testing.T) {
	binary, store, executable, session := capabilityOwnerFixture(t)
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--send", session)
	before := readCapabilityOwnerStatus(t, binary, store.Dir)
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, "grant", "external", "--read", session); err == nil {
		t.Fatal("grant accepted executable drift", string(raw))
	}
	drifted := readCapabilityOwnerStatus(t, binary, store.Dir)
	if !reflect.DeepEqual(before.Capabilities, drifted.Capabilities) || drifted.PinMatches == nil || *drifted.PinMatches {
		t.Fatal("failed grant changed authority or hid drift", drifted)
	}
	capabilityCLIOK(t, binary, store.Dir, "enable", "external")
	capabilityCLIOK(t, binary, store.Dir, "disable", "external")
	stale := readCapabilityOwnerStatus(t, binary, store.Dir)
	if stale.CapabilitiesBound == nil || *stale.CapabilitiesBound {
		t.Fatal("repin silently rebound old grants", stale)
	}
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--inbox-read")
	rebound := readCapabilityOwnerStatus(t, binary, store.Dir)
	assertCapabilityOwnerRebound(t, &rebound, before.Capabilities.Revision)
	capabilityCLIOK(t, binary, store.Dir, "remove", "external")
	capabilityCLIOK(t, binary, store.Dir, "add", "external", "--exec", executable)
	added := readCapabilityOwnerStatus(t, binary, store.Dir)
	if added.ID == before.ID || added.Capabilities != nil || added.CapabilitiesBound != nil {
		t.Fatal("re-added installation inherited old grants", added)
	}
}

func TestPluginCapabilityOwnerCLIRejectsMalformedAndOverLimit(t *testing.T) {
	binary, store, _, session := capabilityOwnerFixture(t)
	cases := [][]string{{"grant", "external"}, {"grant", "external", "--read", "display-name"}, {"grant", "external", "--send", "plugin:00000000-0000-4000-8000-000000000001"}, {"grant", "external", "--read", session, "--read", session}, {"revoke", "external", "--read", session}}
	for _, args := range cases {
		if raw, err := pluginOwnerCLI(t, binary, store.Dir, args...); err == nil {
			t.Fatal("accepted malformed capability selection", args, string(raw))
		}
	}
	args := make([]string, 0, 36)
	args = append(args, "grant", "external")
	for i := range 16 {
		args = append(args, "--read", capabilityNativeAddress(i))
	}
	args = append(args, "--send", capabilityNativeAddress(16))
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, args...); err == nil {
		t.Fatal("accepted over-limit capability targets", string(raw))
	}
	if status := readCapabilityOwnerStatus(t, binary, store.Dir); status.Capabilities != nil {
		t.Fatal("rejected selection wrote capabilities", status)
	}
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--inbox-read")
	path := filepath.Join(store.Dir, "plugins", "external.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	section, ok := record["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("missing persisted capability section")
	}
	section["unknown_permission"] = true
	raw, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := pluginOwnerCLI(t, binary, store.Dir, "status", "external"); err == nil {
		t.Fatal("CLI accepted unknown capability field", string(output))
	}
}

func capabilityNativeAddress(i int) string {
	return fmt.Sprintf("codex:00000000-0000-4000-8000-%012d", i+1)
}

func assertCapabilityOwnerFullGrant(t *testing.T, status *capabilityOwnerStatus, session string) *bus.PluginCapabilities {
	t.Helper()
	c := status.Capabilities
	if c == nil || status.Launch == nil || c.Version != 1 || c.Installation != status.ID || c.PinSHA256 != status.Launch.PinSHA256 || c.Revision == "" || !c.InboxRead || !slices.Equal(c.Read, []string{session}) || !slices.Equal(c.Send, []string{session}) || !slices.Equal(c.InboxFrom, []string{session}) || status.CapabilitiesBound == nil || !*status.CapabilitiesBound {
		t.Fatal("status lost the owner grant or binding", status)
	}
	return c
}

func assertCapabilityOwnerRebound(t *testing.T, status *capabilityOwnerStatus, oldRevision string) {
	t.Helper()
	if status.Capabilities == nil || status.CapabilitiesBound == nil || !*status.CapabilitiesBound || len(status.Capabilities.Send) != 0 || status.Capabilities.Revision == oldRevision {
		t.Fatal("new grant inherited stale pin authority", status)
	}
}
