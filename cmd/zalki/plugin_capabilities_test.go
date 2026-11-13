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
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

type capabilityOwnerStatus struct {
	pluginfeed.PluginFeedStatus
	PinMatches *bool `json:"pin_matches"`
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
	capabilityCLIOK(t, binary, store.Dir, "configure", "external", "--scope", "all")
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

func TestPluginAccessOwnerCLIReplacementStatusAndRevoke(t *testing.T) {
	binary, store, _, session := capabilityOwnerFixture(t)
	initial := readCapabilityOwnerStatus(t, binary, store.Dir)
	if initial.Scope != "all" || initial.Access.Status || initial.Access.Receive || len(initial.Access.Read)+len(initial.Access.Send) != 0 {
		t.Fatal("configure implicitly granted access", initial)
	}
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--status", "--read", session, "--send", session, "--receive")
	first := readCapabilityOwnerStatus(t, binary, store.Dir)
	if a := first.Access; !a.Status || !a.Receive || !slices.Equal(a.Read, []string{session}) || !slices.Equal(a.Send, []string{session}) || first.Revision == "" {
		t.Fatal("status lost the owner grant", first)
	}
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--status", "--receive")
	second := readCapabilityOwnerStatus(t, binary, store.Dir)
	if a := second.Access; !a.Receive || len(a.Read)+len(a.Send) != 0 {
		t.Fatal("grant did not replace omitted access", second)
	}
	if second.Revision == first.Revision {
		t.Fatal("a different grant kept the revision")
	}
	assertEnabledRefusesChangesAndRevokeClears(t, binary, store.Dir, session)
}

// assertEnabledRefusesChangesAndRevokeClears: an enabled plugin takes no
// grant or scope change; revoke clears all access and keeps the scope.
func assertEnabledRefusesChangesAndRevokeClears(t *testing.T, binary, state, session string) {
	t.Helper()
	capabilityCLIOK(t, binary, state, "enable", "external")
	if raw, err := pluginOwnerCLI(t, binary, state, "grant", "external", "--status", "--read", session); err == nil {
		t.Fatal("granted while enabled", string(raw))
	}
	if raw, err := pluginOwnerCLI(t, binary, state, "configure", "external", "--scope", "none"); err == nil {
		t.Fatal("scope changed while enabled", string(raw))
	}
	capabilityCLIOK(t, binary, state, "revoke", "external")
	final := readCapabilityOwnerStatus(t, binary, state)
	if final.Enabled || final.Access.Status || final.Access.Receive || final.Scope != "all" {
		t.Fatal("revoke left access or changed the scope", final)
	}
}

func TestPluginAccessOwnerCLIScopeKeepsGrants(t *testing.T) {
	binary, store, _, session := capabilityOwnerFixture(t)
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--status", "--read", session)
	granted := readCapabilityOwnerStatus(t, binary, store.Dir)
	capabilityCLIOK(t, binary, store.Dir, "configure", "external", "--scope", "none")
	after := readCapabilityOwnerStatus(t, binary, store.Dir)
	if !reflect.DeepEqual(granted.Access, after.Access) || after.Scope != "none" || after.Revision == granted.Revision {
		t.Fatal("scope change discarded access or kept the revision", after)
	}
	for _, scope := range []string{"ALL", "", "*", "some"} {
		if raw, err := pluginOwnerCLI(t, binary, store.Dir, "configure", "external", "--scope", scope); err == nil {
			t.Fatal("accepted scope", scope, string(raw))
		}
	}
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, "configure", "external", "--session", session); err == nil {
		t.Fatal("the old --session flag still works", string(raw))
	}
}

func TestPluginAccessOwnerCLIPinAndReaddFence(t *testing.T) {
	binary, store, executable, session := capabilityOwnerFixture(t)
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--status", "--send", session)
	before := readCapabilityOwnerStatus(t, binary, store.Dir)
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, "grant", "external", "--status", "--read", session); err == nil {
		t.Fatal("grant accepted executable drift", string(raw))
	}
	drifted := readCapabilityOwnerStatus(t, binary, store.Dir)
	if !reflect.DeepEqual(before.Access, drifted.Access) || drifted.PinMatches == nil || *drifted.PinMatches {
		t.Fatal("failed grant changed authority or hid drift", drifted)
	}
	capabilityCLIOK(t, binary, store.Dir, "enable", "external")
	capabilityCLIOK(t, binary, store.Dir, "disable", "external")
	repinned := readCapabilityOwnerStatus(t, binary, store.Dir)
	if repinned.Revision == before.Revision {
		t.Fatal("a new pin kept the revision", repinned)
	}
	capabilityCLIOK(t, binary, store.Dir, "remove", "external")
	capabilityCLIOK(t, binary, store.Dir, "add", "external", "--exec", executable)
	added := readCapabilityOwnerStatus(t, binary, store.Dir)
	if added.ID == before.ID || added.Access.Status || len(added.Access.Send) != 0 || added.Scope != "" {
		t.Fatal("re-added installation inherited old access", added)
	}
}

func TestPluginAccessOwnerCLIRejectsMalformedAndOverLimit(t *testing.T) {
	binary, store, _, session := capabilityOwnerFixture(t)
	cases := [][]string{{"grant", "external"}, {"grant", "external", "--read", "display-name"}, {"grant", "external", "--send", "plugin:00000000-0000-4000-8000-000000000001"}, {"grant", "external", "--read", session, "--read", session}, {"revoke", "external", "--read", session}, {"grant", "external", "--receive", session}}
	for _, args := range cases {
		if raw, err := pluginOwnerCLI(t, binary, store.Dir, args...); err == nil {
			t.Fatal("accepted malformed access selection", args, string(raw))
		}
	}
	args := make([]string, 0, 36)
	args = append(args, "grant", "external", "--status")
	for i := range 16 {
		args = append(args, "--read", capabilityNativeAddress(i))
	}
	args = append(args, "--send", capabilityNativeAddress(16))
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, args...); err == nil {
		t.Fatal("accepted over-limit named sessions", string(raw))
	}
	if status := readCapabilityOwnerStatus(t, binary, store.Dir); status.Access.Status || len(status.Access.Read) != 0 {
		t.Fatal("rejected selection wrote access", status)
	}
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--status", "--receive")
	path := filepath.Join(store.Dir, "plugins", "external.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	section, ok := record["access"].(map[string]any)
	if !ok {
		t.Fatal("missing persisted access section")
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
		t.Fatal("CLI accepted an unknown access field", string(output))
	}
}

func capabilityNativeAddress(i int) string {
	return fmt.Sprintf("codex:00000000-0000-4000-8000-%012d", i+1)
}
