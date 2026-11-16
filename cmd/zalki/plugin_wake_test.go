package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// R7-H2: a confirmed `plugins enable` wakes an idle supervisor with the
// non-killing kickstart, only when the login item is ours, loaded and
// matching; otherwise it changes nothing and says what is true.
func TestPluginEnableWakesSupervisor(t *testing.T) {
	previous := pluginAccessTerminal
	pluginAccessTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { pluginAccessTerminal = previous })
	for _, tc := range []struct {
		name    string
		report  install.Report
		changes []string
		line    string
	}{
		{"owned and loaded", reportOwned(thisBinary(t)), []string{"upgrade"}, fmt.Sprintf(wakeEnableStarted, "demo")},
		{"not set up", reportNone, nil, fmt.Sprintf(wakeEnableNotSetUp, "demo")},
		{"refused row", reportDisabled, nil, fmt.Sprintf(wakeEnableUnknown, "demo")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, fake := defaultHome(t, tc.report)
			for _, args := range [][]string{{"plugins", "add", "demo", "--exec", feedFixture(t)}, {"plugins", "grant", "demo", "--status"}} {
				if out, err := runDefault(t, home, installNoTerminal, "", args...); err != nil {
					t.Fatal(args, out, err)
				}
			}
			fake.changes = nil
			out, err := runDefault(t, home, installNoTerminal, "", "plugins", "enable", "demo")
			if !slices.Equal(fake.changes, tc.changes) {
				t.Fatalf("an enable did not wake the supervisor as allowed: %v, want %v", fake.changes, tc.changes)
			}
			if err != nil || out != "Enabled demo.\n"+tc.line+"\n" {
				t.Fatalf("an enable did not say what happens next: %q %v", out, err)
			}
			if strings.Contains(out, "plugins run demo") && tc.changes != nil {
				t.Fatal("a woken supervisor was answered with a manual run")
			}
		})
	}
}

// R7-M3: with a running supervisor (its lock held) the install line says
// that it starts the plugin, never "Start it with: zalki plugins run".
func TestPluginInstallLineWithRunningSupervisor(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	if err := os.MkdirAll(filepath.Join(store.Dir, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, busy, err := acquireSupervisorLock(store.Dir)
	if err != nil || busy || lock == nil {
		t.Fatal("the test could not hold the supervisor lock", busy, err)
	}
	defer lock.Close()
	line := wakeInstallLine(wakeSupervisor(t.Context(), nil, store.Dir), "demo")
	if line != fmt.Sprintf(wakeInstallRunning, "demo", "demo") {
		t.Fatalf("a running supervisor was answered with %q", line)
	}
}

// R7-L2: an unfinished login-item change names its one repair.
func TestPluginLoginItemPendingNamesRepair(t *testing.T) {
	if !strings.Contains(loginItemPendingReason, "run zalki install --apply in your own terminal") {
		t.Fatalf("the unfinished-change reason names no repair: %q", loginItemPendingReason)
	}
}
