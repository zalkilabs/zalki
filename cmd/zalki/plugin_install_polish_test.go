package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one-question screen names only the standard access the plugin asked
// for, in every live session.
func TestPluginInstallAccessLine(t *testing.T) {
	lines, err := reviewAccessLines(pluginReviewPlan{access: pluginReviewAccess{status: true, receive: true}})
	if err != nil || strings.Join(lines, "\n") != "  see the status of all sessions, now and later\n  get messages that sessions send to it, and answer the sender" {
		t.Fatalf("%q %v", lines, err)
	}
	lines, err = reviewAccessLines(pluginReviewPlan{access: pluginReviewAccess{status: true}})
	if err != nil || strings.Contains(strings.Join(lines, "\n"), "receive") {
		t.Fatal(lines, err)
	}
}

// The merged warning keeps every security statement of the two old texts, and
// the shared trust notice stays word for word for the other commands.
func TestPluginInstallWarningKeepsEveryStatement(t *testing.T) {
	for _, want := range []string{"no sandbox", "build runs now", "plugin runs later", "OS user access", "normal environment", "cannot undo", "read and change all your Zalki state", "filters only what Zalki exposes, not file access", "only code you trust"} {
		if !strings.Contains(installWarning, want) {
			t.Fatalf("warning lost %q", want)
		}
	}
	if strings.Count(installWarning, "\n") != 1 {
		t.Fatal("warning is not two lines")
	}
	if pluginTrustNotice != "External plugins run with your OS user permissions. Access and scope filter what Zalki exposes; they do not restrict filesystem access. A plugin can read and change all of your Zalki state. Run only code you trust." {
		t.Fatal("shared trust notice changed")
	}
}

func TestPluginInstallBuildTailKeepsLines(t *testing.T) {
	got := plainLines("# demo\r\n./main.go:3:15: undefined: x\nevil \x1b[2J\n")
	if got != "# demo\n./main.go:3:15: undefined: x\n\"evil \\x1b[2J\"\n" {
		t.Fatalf("%q", got)
	}
}

func TestPluginOwnerCommandsSayWhatTheyDid(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	exe := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "fake-notifier", "--exec", feedFixture(t)}, "Added fake-notifier (disabled).\n"},
		{[]string{"configure", "fake-notifier", "--scope", "all"}, "Configured fake-notifier: scope all.\n"},
		{[]string{"grant", "fake-notifier", "--status"}, "Recorded access for fake-notifier.\n"},
		{[]string{"enable", "fake-notifier"}, "Enabled fake-notifier.\n"},
		{[]string{"disable", "fake-notifier"}, "Disabled fake-notifier.\n"},
		{[]string{"remove", "fake-notifier"}, "Removed fake-notifier.\n"},
		{[]string{"add", "external", "--exec", exe}, "Added external (disabled).\n"},
		{[]string{"grant", "external", "--status", "--receive"}, "Recorded access for external.\n"},
		{[]string{"revoke", "external"}, "Revoked all access of external; it is disabled.\n"},
	} {
		raw, err := pluginOwnerLocal(t, store.Dir, step.args...)
		if err != nil || string(raw) != step.want {
			t.Fatalf("%v: got %q, want %q (%v)", step.args, raw, step.want, err)
		}
	}
}

func TestPluginInstallBuildFailureShowsRealLines(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	folder := fakeGoFolder(t, `printf '# demo\n./main.go:3:15: undefined: x\n' >&2; exit 1`)
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", folder)
	if err == nil || !strings.Contains(output, "# demo\n./main.go:3:15: undefined: x\n") || strings.Contains(output, `\n`) {
		t.Fatalf("build output not shown line by line: %v\n%s", err, output)
	}
}

// The read grant opens every message sent to the session, whoever sent it, so
// the owner-facing text must say so.
func TestPluginGrantReadHelpNamesAnySender(t *testing.T) {
	cmd := newPluginCapabilityCommand(&commandOptions{}, "grant")
	if flag := cmd.Flags().Lookup("read"); flag == nil || !strings.Contains(flag.Usage, "received messages") || !strings.Contains(flag.Usage, "from any sender") {
		t.Fatal("grant --read help hides other senders")
	}
	if newPluginInstallCommand(&commandOptions{}).Flags().Lookup("read") != nil {
		t.Fatal("install still takes --read")
	}
}
