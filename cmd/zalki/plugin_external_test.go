package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestExternalPluginOwnerAddAndStatusMismatch(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	file := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"$HOME", "$(not-executed)", "literal space"}
	command := append([]string{"add", "external", "--exec", file, "--"}, args...)
	if raw, err := pluginOwnerLocal(t, store.Dir, command...); err != nil {
		t.Fatal(string(raw), err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "configure", "external", "--scope", "all"); err != nil {
		t.Fatal(string(raw), err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "grant", "external", "--status"); err != nil {
		t.Fatal(string(raw), err)
	}
	status := readExternalOwnerStatus(t, store.Dir)
	if status.Enabled || status.Launch == nil || status.PinMatches == nil || !*status.PinMatches || !slices.Equal(status.Launch.Args, args) {
		t.Fatal("owner add lost literal/pinned launch", status)
	}
	t.Run("mismatch-and-repin", (&externalOwnerRepinCheck{store, file, status}).run)
}

type externalOwnerStatus struct {
	pluginfeed.PluginFeedStatus
	PinMatches *bool  `json:"pin_matches"`
	PinError   string `json:"pin_error"`
}

func readExternalOwnerStatus(t *testing.T, state string) externalOwnerStatus {
	t.Helper()
	raw, err := pluginOwnerLocal(t, state, "status", "external")
	var status externalOwnerStatus
	if err != nil || json.Unmarshal(raw, &status) != nil {
		t.Fatal("external status failed", string(raw), err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"outbox", "max_events"} {
		if _, ok := fields[name]; ok {
			t.Fatal("external status claimed reference-only outbox cap", name)
		}
	}
	return status
}

func TestExternalPluginOwnerRequiresExecAndArgvSeparator(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	file := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "external"}, {"add", "external", "--exec", file, "unseparated"}, {"add", "fake-notifier", "--", "unexpected"}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, args...); err == nil {
			t.Fatal("invalid add accepted", args, string(raw))
		}
	}
}

func TestExternalPluginOwnerReportsStoredArgvDrift(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	file := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "add", "external", "--exec", file, "--", "original"); err != nil {
		t.Fatal(string(raw), err)
	}
	path := filepath.Join(store.Dir, "plugins", "external.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	var launch pluginfeed.PluginLaunch
	if err := json.Unmarshal(record["launch"], &launch); err != nil {
		t.Fatal(err)
	}
	launch.Args = []string{"changed"}
	record["launch"], err = json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
	status := readExternalOwnerStatus(t, store.Dir)
	if status.PinMatches == nil || *status.PinMatches || status.PinError == "" {
		t.Fatal("stored argv drift was not visible", status)
	}
}

type externalOwnerRepinCheck struct {
	store    bus.Store
	file     string
	original externalOwnerStatus
}

func (check *externalOwnerRepinCheck) run(t *testing.T) {
	store, file, status := check.store, check.file, check.original
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	changed := readExternalOwnerStatus(t, store.Dir)
	if changed.PinMatches == nil || *changed.PinMatches || changed.PinError == "" || changed.Launch.SHA256 != status.Launch.SHA256 {
		t.Fatal("status concealed mismatch or repinned", changed)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "enable", "external"); err != nil {
		t.Fatal(string(raw), err)
	}
	repinned := readExternalOwnerStatus(t, store.Dir)
	if repinned.PinMatches == nil || !*repinned.PinMatches || repinned.Launch.SHA256 == status.Launch.SHA256 {
		t.Fatal("enable did not pin current executable", repinned)
	}
}
