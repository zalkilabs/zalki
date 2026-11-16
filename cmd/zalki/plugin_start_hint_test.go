package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

// After a plugin change, a background supervisor starts an enabled plugin itself, and
// a foreground run next to it only competes for the lease. So the run step
// is offered only for the case without one, and enable stays a step of its own.
func TestPluginStartHintsAllowForTheSupervisor(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		grant   string
		want    string
	}{
		{true, "", "If no background supervisor runs, start it with: zalki plugins run demo"},
		{false, "", "Enable it with: zalki plugins enable demo; then, if no background supervisor runs, start it with: zalki plugins run demo"},
		{false, "--status", "Enable it with: zalki plugins grant demo --status && zalki plugins enable demo; then, if no background supervisor runs, start it with: zalki plugins run demo"},
	} {
		if got := updateStartHint("demo", tc.enabled, tc.grant); got != tc.want {
			t.Fatalf("update start hint (enabled %t): %q, want %q", tc.enabled, got, tc.want)
		}
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := pluginAccessRecorded(cmd, nil, "demo"); err != nil || out.String() != "Recorded access for demo. If no background supervisor runs, stop any running copy, then start it with: zalki plugins run demo\n" {
		t.Fatalf("access hint: %q %v", out.String(), err)
	}
}
