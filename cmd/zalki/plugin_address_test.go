package main

import (
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// Show and list print the address sessions send messages to
// ("plugin:<installation UUID>"), and show uses the same run phrase as list.
func TestPluginShowAndListPrintAddress(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	if raw, err := pluginOwnerLocal(t, store.Dir, "add", "demo", "--exec", feedFixture(t)); err != nil {
		t.Fatal(string(raw), err)
	}
	address := "plugin:" + installationID(t, store.Dir, "demo")
	show := func() []string {
		t.Helper()
		raw, err := pluginOwnerLocal(t, store.Dir, "show", "demo")
		if err != nil {
			t.Fatal(string(raw), err)
		}
		return strings.Split(string(raw), "\n")
	}
	if lines := show(); len(lines) < 3 || lines[1] != "Address: "+address || lines[2] != "Disabled" {
		t.Fatalf("show does not print the plugin address: %q", lines)
	}
	if got := strings.Join(show(), "\n"); !strings.Contains(got, "Enable it with: zalki plugins enable demo; then, if no background supervisor runs, start it with: zalki plugins run demo") {
		t.Fatalf("show lost the disabled plugin's conditional start guidance: %q", got)
	}
	for _, args := range [][]string{{"grant", "demo", "--status"}, {"enable", "demo"}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, args...); err != nil {
			t.Fatal(args, string(raw), err)
		}
	}
	if lines := show(); len(lines) < 3 || lines[2] != "Enabled; "+runPhraseStopped {
		t.Fatalf("show does not use the run phrase: %q", lines)
	}
	if got := strings.Join(show(), "\n"); !strings.Contains(got, "If no background supervisor runs, start it with: zalki plugins run demo") {
		t.Fatalf("show lost the enabled plugin's conditional start guidance: %q", got)
	}
	raw, err := pluginOwnerLocal(t, store.Dir, "list")
	if err != nil || !strings.Contains(string(raw), "; address "+address+"\n") {
		t.Fatalf("list does not print the plugin address: %q %v", raw, err)
	}
	if got := pluginAddress(pluginfeed.PluginFeedStatus{ID: "not-a-uuid"}); got != "" {
		t.Fatalf("an invalid installation ID was shown as an address: %q", got)
	}
}
