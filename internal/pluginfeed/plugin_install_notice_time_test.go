package pluginfeed

import "testing"

// The text shown to the agent uses local time with its offset; the stored
// InstalledAt stays UTC.
func TestSessionInstallNoticeShowsLocalTimeWithOffset(t *testing.T) {
	p := pluginRecord{Name: "demo", Install: &PluginInstallReceipt{InstalledAt: "2026-11-22T17:35:23.123456789Z", InstalledBy: "terminal"}}
	const want = "New plugin: demo, installed 19:35 +02:00 by terminal. See: zalki plugins list"
	if got := sessionInstallNotice(p); got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	if p.Install.InstalledAt != "2026-11-22T17:35:23.123456789Z" {
		t.Fatal("the stored install time changed")
	}
}
