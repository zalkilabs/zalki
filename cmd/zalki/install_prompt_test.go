package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// install and uninstall list the files they will change, ask one [y/N] question
// and change nothing unless the answer is yes.

const promptQuestion = "Apply these changes? [y/N]"

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestInstallListsTheFilesAsksOnceAndAppliesOnYes(t *testing.T) {
	home := presenceHome(t, ".claude")
	withTerminal(t, installPlainLines)
	out, diagnostic, err := executeBare(t, home, "y\n", "install", "--state", filepath.Join(home, "state"))
	claude := filepath.Join(home, ".claude.json")
	if err != nil || diagnostic != "" {
		t.Fatalf("error %v, diagnostic %q\n%s", err, diagnostic, out)
	}
	for _, want := range []string{
		"Zalki will change these files and keep a backup of each:\n  Claude: ~/.claude.json, ~/.claude/settings.json\n",
		promptQuestion,
		"Updated: " + claude + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "[y/N]") != 1 || !fileExists(t, claude) {
		t.Fatalf("want one question and a configured client:\n%s", out)
	}
}

func TestInstallThatIsNotAnsweredYesChangesNothing(t *testing.T) {
	for _, input := range []string{"n\n", "\n", ""} {
		home := presenceHome(t, ".claude")
		withTerminal(t, installPlainLines)
		out, diagnostic, err := executeBare(t, home, input, "install", "--state", filepath.Join(home, "state"))
		if err != nil || diagnostic != "" || !strings.HasSuffix(out, promptQuestion+" \nNothing changed.\n") {
			t.Fatalf("input %q: error %v, diagnostic %q\n%s", input, err, diagnostic, out)
		}
		if fileExists(t, filepath.Join(home, ".claude.json")) || fileExists(t, filepath.Join(home, ".claude", "settings.json")) {
			t.Fatalf("input %q: a declined install wrote a file", input)
		}
	}
}

func TestInstallWithoutATerminalRefusesAndNamesYes(t *testing.T) {
	home := presenceHome(t, ".claude")
	withTerminal(t, installNoTerminal)
	out, _, err := executeBare(t, home, "y\n", "install", "--state", filepath.Join(home, "state"))
	const want = "zalki install needs a terminal to ask; run zalki install --yes to change the files without asking"
	if err == nil || err.Error() != want || out != "" {
		t.Fatalf("error %v, output %q; want %q and no output", err, out, want)
	}
	if fileExists(t, filepath.Join(home, ".claude.json")) {
		t.Fatal("a refused install wrote a file")
	}
}

func TestInstallWithYesDoesNotAskAndDryRunWinsOverYes(t *testing.T) {
	home := presenceHome(t, ".claude")
	withTerminal(t, installPlainLines)
	state := filepath.Join(home, "state")
	out, _, err := executeBare(t, home, "", "install", "--dry-run", "--yes", "--state", state)
	if err != nil || strings.Contains(out, "[y/N]") || !strings.Contains(out, `"dry_run":true`) || fileExists(t, filepath.Join(home, ".claude.json")) {
		t.Fatalf("dry run with --yes: error %v\n%s", err, out)
	}
	out, _, err = executeBare(t, home, "", "install", "--yes", "--state", state)
	if err != nil || strings.Contains(out, "[y/N]") || !fileExists(t, filepath.Join(home, ".claude.json")) {
		t.Fatalf("install --yes: error %v\n%s", err, out)
	}
}

// With nothing to change there is nothing to ask: a second install needs no terminal.
func TestInstallWithNothingToChangeDoesNotAsk(t *testing.T) {
	home := presenceHome(t, ".claude")
	state := filepath.Join(home, "state")
	if _, _, err := executeBare(t, home, "", "install", "--yes", "--state", state); err != nil {
		t.Fatal(err)
	}
	withTerminal(t, installNoTerminal)
	out, _, err := executeBare(t, home, "", "install", "--state", state)
	if err != nil || strings.Contains(out, "[y/N]") {
		t.Fatalf("error %v\n%s", err, out)
	}
}

func TestUninstallAsksOnceAndKeepsEverythingUnlessYes(t *testing.T) {
	home := presenceHome(t, ".claude")
	state := filepath.Join(home, "state")
	if _, _, err := executeBare(t, home, "", "install", "--yes", "--state", state); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(state, "installation.json")
	withTerminal(t, installPlainLines)
	out, _, err := executeBare(t, home, "n\n", "uninstall", "--state", state)
	if err != nil || !strings.HasSuffix(out, promptQuestion+" \nNothing changed.\n") || !fileExists(t, record) {
		t.Fatalf("declined: error %v\n%s", err, out)
	}
	out, _, err = executeBare(t, home, "y\n", "uninstall", "--state", state)
	if err != nil || strings.Count(out, "[y/N]") != 1 || fileExists(t, record) {
		t.Fatalf("accepted: error %v\n%s", err, out)
	}
}

func TestUninstallWithoutATerminalRefusesAndNamesYes(t *testing.T) {
	home := presenceHome(t, ".claude")
	state := filepath.Join(home, "state")
	if _, _, err := executeBare(t, home, "", "install", "--yes", "--state", state); err != nil {
		t.Fatal(err)
	}
	withTerminal(t, installNoTerminal)
	_, _, err := executeBare(t, home, "", "uninstall", "--state", state)
	if err == nil || !strings.Contains(err.Error(), "run zalki uninstall --yes") || !fileExists(t, filepath.Join(state, "installation.json")) {
		t.Fatalf("error %v; want a refusal that names --yes, and the record kept", err)
	}
}

func TestTheApplyFlagIsGoneAndTheErrorSaysWhatToDo(t *testing.T) {
	home := presenceHome(t, ".claude")
	for _, command := range []string{"install", "uninstall"} {
		_, _, err := executeBare(t, home, "", command, "--apply")
		want := "--apply is no longer needed; run zalki " + command + " and answer the question, or use --yes"
		if err == nil || err.Error() != want {
			t.Fatalf("%s --apply: error %v, want %q", command, err, want)
		}
	}
}

func TestSetupWithoutOutPrintsThePreviewAndWritesNothing(t *testing.T) {
	home := presenceHome(t, ".claude")
	t.Chdir(home)
	out, _, err := executeBare(t, home, "", "setup", "--state", filepath.Join(home, "state"))
	if err != nil || !strings.Contains(out, `"dry_run":true`) || fileExists(t, filepath.Join(home, "generated")) || fileExists(t, filepath.Join(home, ".claude.json")) {
		t.Fatalf("error %v\n%s", err, out)
	}
}

func TestSetupRefusesAFolderThatIsNotEmptyBeforeItWritesAnything(t *testing.T) {
	home := presenceHome(t, ".claude")
	drafts := filepath.Join(home, "drafts")
	if err := os.MkdirAll(drafts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drafts, "mine.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := executeBare(t, home, "", "setup", "--state", filepath.Join(home, "state"), "--out", drafts)
	if err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("error %v; want a refusal of a folder that is not empty", err)
	}
	entries, _ := os.ReadDir(drafts)
	if len(entries) != 1 {
		t.Fatalf("the folder has %d entries; want only the user's file", len(entries))
	}
}

// One key is enough on a terminal that sends keys one by one.
func TestInstallTakesOneKeyOnARawTerminal(t *testing.T) {
	home := presenceHome(t, ".claude")
	withTerminal(t, installRawKeys)
	out, _, err := executeBare(t, home, "y", "install", "--state", filepath.Join(home, "state"))
	if err != nil || !fileExists(t, filepath.Join(home, ".claude.json")) {
		t.Fatalf("error %v\n%s", err, out)
	}
}

// The built program, run with no terminal on its input, refuses without --yes and
// changes nothing; with --yes it configures the client.
func TestTheBuiltProgramRefusesWithoutATerminalAndInstallsWithYes(t *testing.T) {
	binary := checkRealBinary(t)
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "state")
	_, diagnostic, code := runInstalledBinary(t, binary, home, "install", "--state", state)
	if code != 1 || !strings.Contains(diagnostic, "run zalki install --yes") || fileExists(t, filepath.Join(home, ".claude.json")) {
		t.Fatalf("exit %d, diagnostic %q, or a file was written", code, diagnostic)
	}
	out, _, code := runInstalledBinary(t, binary, home, "install", "--yes", "--state", state)
	if code != 0 || !fileExists(t, filepath.Join(home, ".claude.json")) || strings.Contains(out, "[y/N]") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// A dry run of uninstall prints the preview and changes nothing, also with --yes.
func TestUninstallDryRunChangesNothing(t *testing.T) {
	home := presenceHome(t, ".claude")
	state := filepath.Join(home, "state")
	if _, _, err := executeBare(t, home, "", "install", "--yes", "--state", state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--dry-run"}, {"--dry-run", "--yes"}} {
		out, _, err := executeBare(t, home, "", append([]string{"uninstall", "--state", state}, args...)...)
		if err != nil || !strings.Contains(out, `"dry_run":true`) || strings.Contains(out, "[y/N]") {
			t.Fatalf("%v: error %v\n%s", args, err, out)
		}
		after, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
		if string(after) != string(before) || !fileExists(t, filepath.Join(state, "installation.json")) {
			t.Fatalf("%v: a dry run changed files", args)
		}
	}
}

// The question lists the files one line for each client, in the order of the client
// names, with ~ for the home folder; a file of no client comes last, under Zalki.
func TestTheQuestionListsTheFilesGroupedByClient(t *testing.T) {
	home := "/home/ada"
	report := install.Report{
		Paths: []string{home + "/.cursor/mcp.json", home + "/.claude.json", "/opt/state/other.json", home + "/.claude/settings.json", home + "/.codex/config.toml"},
		Clients: map[string]string{
			home + "/.cursor/mcp.json":      "Cursor",
			home + "/.claude.json":          "Claude",
			home + "/.claude/settings.json": "Claude",
			home + "/.codex/config.toml":    "Codex",
		},
	}
	want := []string{
		"Claude: ~/.claude.json, ~/.claude/settings.json",
		"Codex: ~/.codex/config.toml",
		"Cursor: ~/.cursor/mcp.json",
		"Zalki: /opt/state/other.json",
	}
	if got := pathsByClient(report, home); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines %q, want %q", got, want)
	}
}

// A client that is skipped or not found is named before the question, so a user
// who answers n knows what was left out and why. After y the lines are not
// printed a second time.
func TestSkippedAndMissingClientsAreNamedBeforeTheQuestion(t *testing.T) {
	for _, input := range []string{"n\n", "y\n"} {
		home := presenceHome(t, ".claude", ".cursor")
		broken := filepath.Join(home, ".cursor", "mcp.json")
		if err := os.WriteFile(broken, []byte(`{bad`), 0o600); err != nil {
			t.Fatal(err)
		}
		withTerminal(t, installPlainLines)
		out, _, _ := executeBare(t, home, input, "install", "--state", filepath.Join(home, "state"))
		skipped := "Skipped Cursor: " + broken + ": "
		notFound := "Not found: Antigravity, Codex."
		before, _, found := strings.Cut(out, promptQuestion)
		if !found || !strings.Contains(before, skipped) || !strings.Contains(before, notFound) {
			t.Fatalf("input %q: the skipped and missing clients are not named before the question:\n%s", input, out)
		}
		if strings.Count(out, skipped) != 1 || strings.Count(out, notFound) != 1 {
			t.Fatalf("input %q: a line is printed twice:\n%s", input, out)
		}
	}
}
