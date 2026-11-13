package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/plugins"
)

func TestPluginInstallScreenStyles(t *testing.T) {
	sessions := []model.Session{{ID: "codex:a"}, {ID: "codex:b", Name: "work"}}
	screen := newInstallScreen("", plugins.InstallAccess{Status: true}, sessions)
	lines := screen.render(screenLines)
	for _, want := range []string{"Access per session: changes = see state and message status, no text\n", "1. (no name)  codex:a\n", "2. work  codex:b\n"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("line mode lacks %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "[ ]") || strings.Contains(lines, "own inbox") || strings.Contains(lines, "send =") || strings.Contains(lines, "read =") {
		t.Fatalf("line mode shows boxes or unrequested kinds:\n%s", lines)
	}
	if keys := screen.render(screenKeys); !strings.Contains(keys, "  [ ] changes  2. work  codex:b") || strings.Contains(keys, "own inbox") {
		t.Fatalf("checkbox screen lost its boxes:\n%s", keys)
	}
	if plan := screen.render(screenPlan); !strings.Contains(plan, "  [ ] changes  1. (no name)  codex:a") || strings.Contains(plan, "Arrows move") {
		t.Fatalf("--yes plan lost its boxes or shows keys:\n%s", plan)
	}
	if got := screen.numberHelp(); got != "Use numbers from the list (1-2), separated by commas.\n" {
		t.Fatal(got)
	}
}

// The merged warning keeps every security statement of the two old texts, and
// the shared trust notice stays word for word for the other commands.
func TestPluginInstallWarningKeepsEveryStatement(t *testing.T) {
	for _, want := range []string{"no sandbox", "build runs now", "plugin runs later", "OS user access", "normal environment", "cannot undo", "read and change all your Zalki state", "filter only the Zalki feed, not file access", "only code you trust"} {
		if !strings.Contains(installWarning, want) {
			t.Fatalf("warning lost %q", want)
		}
	}
	if strings.Count(installWarning, "\n") != 1 {
		t.Fatal("warning is not two lines")
	}
	if pluginTrustNotice != "External plugins run with your OS user permissions. Session grants filter the Zalki feed; they do not restrict filesystem access. A plugin can read and change all of your Zalki state. Run only code you trust." {
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
	store, session := pluginOwnerFixture(t)
	exe := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "fake-notifier", "--exec", feedFixture(t)}, "Added fake-notifier (disabled).\n"},
		{[]string{"configure", "fake-notifier", "--session", session.ID}, "Configured fake-notifier: 1 session(s).\n"},
		{[]string{"enable", "fake-notifier"}, "Enabled fake-notifier.\n"},
		{[]string{"disable", "fake-notifier"}, "Disabled fake-notifier.\n"},
		{[]string{"remove", "fake-notifier"}, "Removed fake-notifier.\n"},
		{[]string{"add", "external", "--exec", exe}, "Added external (disabled).\n"},
		{[]string{"grant", "external", "--receive", session.ID}, "Recorded permissions for external.\n"},
		{[]string{"revoke", "external"}, "Revoked all permissions of external; it is disabled.\n"},
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
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, " y", folder)
	if err == nil || !strings.Contains(output, "# demo\n./main.go:3:15: undefined: x\n") || strings.Contains(output, `\n`) {
		t.Fatalf("build output not shown line by line: %v\n%s", err, output)
	}
}

// The read grant opens every message sent to the session, whoever sent it, so
// the owner-facing text must say so.
func TestPluginInstallReadLabelNamesAnySender(t *testing.T) {
	screen := newInstallScreen("", plugins.InstallAccess{Status: true, Read: true}, []model.Session{{ID: "codex:a"}})
	if lines := screen.render(screenLines); !strings.Contains(lines, "read = read messages sent to it (from any sender)") {
		t.Fatalf("read legend hides other senders:\n%s", lines)
	}
	for _, cmd := range []*cobra.Command{newPluginInstallCommand(&commandOptions{}), newPluginCapabilityCommand(&commandOptions{}, "grant")} {
		if flag := cmd.Flags().Lookup("read"); flag == nil || !strings.Contains(flag.Usage, "received messages") || !strings.Contains(flag.Usage, "from any sender") {
			t.Fatalf("%s --read help hides other senders", cmd.Name())
		}
	}
}
