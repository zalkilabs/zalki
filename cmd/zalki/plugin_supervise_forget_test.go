package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// forgetFixture is a controller with a banner notifier (no worker) and one
// enabled plugin "keep" whose host runs.
func forgetFixture(t *testing.T) (*supervisor, *hostProcess) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	addEnabled(t, store.Dir(), "keep") // creates the plugins folder first
	s := newNotifySupervisor(t, store.Dir())
	useNotifyFixture(t)
	s.notify = testNotifier(store.Dir(), s.instance, &notifyClock{mono: notifyWindow, wall: stateEpoch})
	scanOnce(t, s)
	keep := s.plugins["keep"]
	if keep == nil || keep.host == nil {
		t.Fatal("the kept plugin did not start")
	}
	return s, keep.host
}

func addEnabled(t *testing.T, state, name string) {
	t.Helper()
	for _, args := range [][]string{{"add", name, "--exec", feedFixture(t)}, {"grant", name, "--status"}, {"enable", name}} {
		ownerRun(t, state, args...)
	}
}

func scanOnce(t *testing.T, s *supervisor) {
	t.Helper()
	if _, err := s.scan(); err != nil {
		t.Fatal(err)
	}
}

// checkForgetBounds: after a scan, one entry per listed name and offered
// holds exactly the remaining installations.
func checkForgetBounds(t *testing.T, s *supervisor, listed int) {
	t.Helper()
	if len(s.plugins) != listed {
		t.Fatalf("the supervisor kept entries for removed plugins: %d entries, %d listed", len(s.plugins), listed)
	}
	remaining := make([]string, 0, len(s.plugins))
	for _, p := range s.plugins {
		remaining = append(remaining, p.run.Installation)
	}
	offered := slices.Sorted(maps.Keys(s.offered))
	slices.Sort(remaining)
	if !slices.Equal(offered, remaining) {
		t.Fatalf("offered kept a removed installation: offered %v, remaining %v", offered, remaining)
	}
}

// Removed names are forgotten after their host is stopped; the
// kept plugin is untouched; a listing failure forgets nothing; an unsaved
// entry stays until a save is confirmed.
func TestPluginSupervisorForgetsRemovedNames(t *testing.T) {
	s, keepHost := forgetFixture(t)
	checkForgetBounds(t, s, 1)
	for i := range 20 {
		name := fmt.Sprintf("gone-%d", i)
		addEnabled(t, s.state, name)
		scanOnce(t, s)
		checkForgetBounds(t, s, 2)
		gone := s.plugins[name]
		if gone == nil || gone.host == nil {
			t.Fatalf("%s did not start", name)
		}
		pid := gone.host.cmd.Process.Pid
		ownerRun(t, s.state, "remove", name)
		scanOnce(t, s)
		if !hostProcessGone(pid) {
			t.Fatal("a removed plugin's host kept running")
		}
		checkForgetBounds(t, s, 1)
		if s.plugins["keep"] == nil || s.plugins["keep"].host != keepHost {
			t.Fatal("the kept plugin was restarted or forgotten")
		}
	}
	checkListingFailureForgetsNothing(t, s)
	checkUnsavedStays(t, s)
	ownerRun(t, s.state, "remove", "keep")
	scanOnce(t, s)
	checkForgetBounds(t, s, 0)
	checkLastUnsavedKeepsRunning(t, s)
}

// checkLastUnsavedKeepsRunning: the last listed plugin is removed and its
// save fails. The scan still reports work (run would otherwise exit and drop
// the fence); after a confirmed save the maps drain and the scan is idle.
func checkLastUnsavedKeepsRunning(t *testing.T, s *supervisor) {
	t.Helper()
	addEnabled(t, s.state, "last")
	scanOnce(t, s)
	ownerRun(t, s.state, "remove", "last")
	saveRun := s.saveRun
	s.saveRun = func(string, string, pluginRunState) error { return errors.New("disk full") }
	for range 2 {
		active, err := s.scan()
		if err != nil || !active || s.plugins["last"] == nil || !s.plugins["last"].unsaved {
			t.Fatalf("a scan with an unsaved removed entry reported nothing active: %t %v", active, err)
		}
	}
	s.saveRun = saveRun
	if active, err := s.scan(); err != nil || active {
		t.Fatalf("a saved, empty controller still reported work: %t %v", active, err)
	}
	checkForgetBounds(t, s, 0)
}

func checkListingFailureForgetsNothing(t *testing.T, s *supervisor) {
	t.Helper()
	addEnabled(t, s.state, "listed")
	scanOnce(t, s)
	ownerRun(t, s.state, "remove", "listed")
	dir := filepath.Join(s.state, "plugins")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	_, err := s.scan()
	if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || len(s.plugins) != 2 || len(s.offered) != 2 {
		t.Fatalf("a listing failure forgot entries: %v, %d entries, %d offered", err, len(s.plugins), len(s.offered))
	}
	scanOnce(t, s)
	checkForgetBounds(t, s, 1)
}

func checkUnsavedStays(t *testing.T, s *supervisor) {
	t.Helper()
	addEnabled(t, s.state, "fence")
	scanOnce(t, s)
	ownerRun(t, s.state, "remove", "fence")
	saveRun := s.saveRun
	s.saveRun = func(dir, name string, state pluginRunState) error {
		if name == "fence" {
			return errors.New("disk full")
		}
		return saveRun(dir, name, state)
	}
	scanOnce(t, s)
	if p := s.plugins["fence"]; p == nil || !p.unsaved {
		t.Fatal("an unsaved removed entry was dropped")
	}
	s.saveRun = saveRun
	scanOnce(t, s)
	checkForgetBounds(t, s, 1)
}
