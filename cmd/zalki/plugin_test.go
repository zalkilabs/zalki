package main

import (
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
