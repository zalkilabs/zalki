package main

import (
	"strings"
	"testing"
)

// The mute help names no release, so it cannot go stale, and
// still says that a listed plugin is access, not a running process.
func TestPluginMuteHelpNamesNoRelease(t *testing.T) {
	for _, muted := range []bool{true, false} {
		long := newPluginMuteCommand(&commandOptions{}, muted).Long
		if strings.Contains(long, "rc.") || strings.Contains(long, "After upgrading") || !strings.Contains(long, "not proof of a running process") {
			t.Fatalf("mute help: %q", long)
		}
	}
}
