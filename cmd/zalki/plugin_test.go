package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginCommandIsExplicitAndDisabledByDefault(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	cmd := newRootCommand(t.TempDir())
	cmd.SetArgs([]string{"--state", state, "--log-level", "off", "plugins", "run", "fake-notifier"})
	if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal("disabled installation executed", err)
	}
	if _, err := os.Stat(filepath.Join(state, "plugins", "fake-notifier.lock")); !os.IsNotExist(err) {
		t.Fatal("disabled command acquired an execution lease", err)
	}
}

func TestReferenceChildDoesNotOpenStateLogger(t *testing.T) {
	state := filepath.Join(t.TempDir(), "must-not-create")
	cmd := newRootCommand(t.TempDir())
	cmd.SetIn(bytes.NewBuffer(nil)) // Deliberately not closable: reject before inspecting FD3.
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--state", state, "--log-file", filepath.Join(state, "logs", "child"), "plugin-notifier", "--outbox", filepath.Join(t.TempDir(), "outbox")})
	if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "closable") {
		t.Fatal("child input validation", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("reference child wrote outside its outbox", err)
	}
}
