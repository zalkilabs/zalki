package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func checkRealBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	return binary
}

func TestPluginCheckRawExecDoesNotCreateRealStateOrLogger(t *testing.T) {
	state := filepath.Join(t.TempDir(), "must-not-create")
	log := filepath.Join(state, "logs", "must-not-create")
	cmd := newRootCommand(t.TempDir())
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--state", state, "--log-level", "debug", "--log-file", log, "plugins", "check", "--exec", feedFixture(t)})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(output.String(), err)
	}
	if !strings.Contains(output.String(), "PASS event-2\n") || !strings.Contains(output.String(), "PASS reaped\n") {
		t.Fatal(output.String())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("raw check created real state/log", err)
	}
}

func TestPluginCheckNamedDisabledDoesNotAdvanceProfileOrUseRealData(t *testing.T) {
	binary := checkRealBinary(t)
	store, _ := pluginOwnerFixture(t)
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, "add", "fake-notifier", "--exec", feedFixture(t)); err != nil {
		t.Fatal(string(raw), err)
	}
	path := filepath.Join(store.Dir, "plugins", "fake-notifier.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := pluginOwnerCLI(t, binary, store.Dir, "check", "fake-notifier")
	if err != nil || !strings.Contains(string(output), "PASS event-2\n") {
		t.Fatal(string(output), err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("check changed enabled/grants/feed/pin", err)
	}
	files, err := os.ReadDir(filepath.Join(store.Dir, "plugins", "fake-notifier.data"))
	if err != nil || len(files) != 0 {
		t.Fatal("check wrote real plugin data", files, err)
	}
}

func TestPluginCheckNamedRefusesDriftAndActiveNameLease(t *testing.T) {
	binary := checkRealBinary(t)
	store, _ := pluginOwnerFixture(t)
	file := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	if err := feed.AddExternalPlugin(file, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := pluginOwnerCLI(t, binary, store.Dir, "check", "external"); err == nil || !strings.Contains(string(output), "FAIL launch") {
		t.Fatal("named check silently repinned/executed drift", string(output), err)
	}
	lease, err := feed.AcquirePluginLease()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if output, err := pluginOwnerCLI(t, binary, store.Dir, "check", "external"); err == nil || !strings.Contains(string(output), "lease unavailable") {
		t.Fatal("named check bypassed active host/child lease", string(output), err)
	}
}

func TestPluginCheckRejectsMixedNameAndRawExecArguments(t *testing.T) {
	for _, args := range [][]string{{"plugins", "check"}, {"plugins", "check", "name", "--exec", "/bin/echo"}, {"plugins", "check", "--exec", "/bin/echo", "unseparated"}} {
		cmd := newRootCommand(t.TempDir())
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		if err := cmd.ExecuteContext(t.Context()); err == nil {
			t.Fatal("ambiguous target accepted", args)
		}
	}
}
