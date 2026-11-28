package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

func labelOfKind(kind string) string { return strings.ToUpper(kind[:1]) + kind[1:] }

func TestSettingsFindingsListEveryMatchingClientAsSetUp(t *testing.T) {
	ok := settingsFindings(install.Report{Configured: []string{"claude", "codex"}}, nil, labelOfKind)
	if len(ok) != 2 || ok[0] != (doctorCheck{Name: "Claude: set up", Status: checkOK}) || ok[1].Name != "Codex: set up" {
		t.Fatalf("matching settings = %+v", ok)
	}
}

func TestSettingsFindingsNameTheClientWhoseFileCannotBeUsed(t *testing.T) {
	bad := settingsFindings(install.Report{
		Configured: []string{"claude"}, Unusable: []string{"Codex"},
		Notes: []string{"Skipped Codex: settings must be a JSON object.", "Next: fix that file, then run zalki upgrade."},
	}, nil, labelOfKind)
	if len(bad) != 2 {
		t.Fatalf("one unusable client: %+v", bad)
	}
	if c := findCheck(t, bad, "Codex:"); c.Status != checkProblem || c.Name != "Codex: its settings file cannot be used (settings must be a JSON object)" || c.Fix != "fix that file, then run zalki upgrade" {
		t.Fatalf("unusable = %+v", c)
	}
	if c := findCheck(t, bad, "Claude"); c.Status != checkOK {
		t.Fatalf("the other client = %+v", c)
	}
	if got := skipReason([]string{"Skipped Other: x."}, "Codex"); got != "" {
		t.Fatalf("a reason for another client: %q", got)
	}
}

func TestSettingsFindingsSayWhenFilesDifferOrCannotBeChecked(t *testing.T) {
	differ := settingsFindings(install.Report{
		Configured: []string{"claude", "codex"}, Paths: []string{"/a", "/b", "/c", "/d"},
		Clients: map[string]string{"/a": "Codex", "/b": "Codex", "/c": "Claude"},
	}, nil, labelOfKind)
	want := []string{"Claude: 1 settings file is out of date", "Codex: 2 settings files are out of date", "Zalki's settings are out of date (1 file)"}
	if len(differ) != 3 {
		t.Fatalf("differing settings = %+v", differ)
	}
	for i, c := range differ {
		if c.Status != checkProblem || c.Name != want[i] || c.Fix != "run zalki upgrade" {
			t.Fatalf("line %d = %+v, want %q", i, c, want[i])
		}
	}
	failed := settingsFindings(install.Report{Configured: []string{"claude"}}, errors.New("lock busy"), labelOfKind)
	if len(failed) != 1 || failed[0].Status != checkProblem || !strings.Contains(failed[0].Name, "lock busy") || failed[0].Fix == "" {
		t.Fatalf("an error = %+v", failed)
	}
}

func doctorSettingsFixture(t *testing.T) (home, state string) {
	t.Helper()
	home = codexDiagnosticTestDir(t)
	state = filepath.Join(home, "state")
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	for name, out := range map[string]string{"claude": "2.1.0 (Claude Code)", "codex": "codex-cli 0.167.2"} {
		fake := filepath.Join(home, "bin", name)
		writeCodexFixture(t, fake, "#!/bin/sh\nprintf '"+out+"\\n'\n")
		if err := os.Chmod(fake, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", filepath.Join(home, "bin"))
	if _, _, err := execute(t, home, "", "install", "--yes", "--state", state); err != nil {
		t.Fatal(err)
	}
	return home, state
}

// snapshotTree lists every file under home with its content hash, except the log folder.
func snapshotTree(t *testing.T, home string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == "logs" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			files[path] = "dir"
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files[path] = fmt.Sprintf("%x", sha256.Sum256(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func doctorWords(t *testing.T, home, state string, args ...string) (string, error) {
	t.Helper()
	out, diagnostic, err := execute(t, home, "", append([]string{"doctor", "--state", state}, args...)...)
	if diagnostic != "" {
		t.Fatalf("stderr %q", diagnostic)
	}
	return out, err
}

// The doctor looks at the settings through the installer's preview. It says each
// client is set up, names the client whose file cannot be used, and says when
// a file differs; in every state it writes nothing.
func TestDoctorChecksTheClientSettingsAndWritesNothing(t *testing.T) {
	home, state := doctorSettingsFixture(t)
	if out, err := doctorWords(t, home, state); err != nil || !strings.Contains(out, "[ok]      Claude: set up") || !strings.Contains(out, "[ok]      Codex: set up") || !strings.Contains(out, "No problems found.") {
		t.Fatalf("set up: %v\n%s", err, out)
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), "'--agent' 'claude'", "'--agent' 'claude' '--edited'", 1)
	if tampered == string(raw) {
		t.Fatalf("no hook command found in %s", raw)
	}
	for _, tc := range []struct {
		name, content, want string
	}{
		{"edited hook", tampered, "[problem] Claude: its settings file cannot be used"},
		{"not JSON", "{ not json", "[problem] Claude: its settings file cannot be used"},
		{"entry removed", "{}", "[problem] Claude: 1 settings file is out of date"},
	} {
		checkBrokenSettings(t, home, state, settings, tc.name, tc.content, tc.want)
	}
}

// checkBrokenSettings puts content into the Claude settings file, runs the
// doctor, and checks the problem line, the exit and that nothing was written.
func checkBrokenSettings(t *testing.T, home, state, settings, name, content, want string) {
	t.Helper()
	if err := os.WriteFile(settings, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, home)
	out, err := doctorWords(t, home, state)
	if !errors.Is(err, errReported) || !strings.Contains(out, want) || !strings.Contains(out, "Next: ") {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	after := snapshotTree(t, home)
	if len(before) != len(after) {
		t.Fatalf("%s: the doctor changed the files: %d before, %d after", name, len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Fatalf("%s: the doctor changed %s", name, path)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "generated")); !os.IsNotExist(err) {
		t.Fatalf("%s: the doctor made a drafts folder", name)
	}
}

func TestKindLabelFallsBackToTheKind(t *testing.T) {
	opts := stubOpts(t, stubSectionAdapter{})
	if kindLabel(opts, "stubsection") != "Stub" || kindLabel(opts, "nosuch") != "nosuch" {
		t.Fatalf("labels: %q %q", kindLabel(opts, "stubsection"), kindLabel(opts, "nosuch"))
	}
}

// A usable record with no client left on the machine is not a problem: the
// settings check is skipped, and the installer's "no supported client" error
// is never shown as a finding.
func TestDoctorWithARecordButNoClientLeftHasNoProblem(t *testing.T) {
	home, state := doctorSettingsFixture(t)
	for _, dir := range []string{".claude", ".codex", ".cursor", ".gemini"} {
		if err := os.RemoveAll(filepath.Join(home, dir)); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{".claude.json"} {
		if err := os.Remove(filepath.Join(home, file)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", filepath.Join(home, "empty"))
	out, err := doctorWords(t, home, state)
	if err != nil || strings.Contains(out, "[problem]") || !strings.Contains(out, "No problems found.") {
		t.Fatalf("exit %v\n%s", err, out)
	}
}
