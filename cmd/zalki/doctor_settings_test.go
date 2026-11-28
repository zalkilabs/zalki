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

func TestSettingsFindingsListEveryMatchingClientAsInPlace(t *testing.T) {
	ok := settingsFindings(install.Report{Configured: []string{"claude", "codex"}}, nil, labelOfKind)
	if len(ok) != 2 || ok[0] != (doctorCheck{Name: "Claude: Zalki settings are in place", Status: checkOK}) || ok[1].Name != "Codex: Zalki settings are in place" {
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
	// Only the client with a differing file is a problem; the other stays in place.
	mixed := settingsFindings(install.Report{
		Configured: []string{"claude", "codex"}, Paths: []string{"/a"}, Clients: map[string]string{"/a": "Codex"},
	}, nil, labelOfKind)
	if len(mixed) != 2 || mixed[0] != (doctorCheck{Name: "Claude: Zalki settings are in place", Status: checkOK}) || mixed[1].Name != "Codex: 1 settings file is out of date" {
		t.Fatalf("one client differs: %+v", mixed)
	}
	// A differing file with no client name: nobody is called in place.
	anon := settingsFindings(install.Report{Configured: []string{"claude"}, Paths: []string{"/z"}}, nil, labelOfKind)
	if len(anon) != 1 || anon[0].Name != "Zalki's settings are out of date (1 file)" {
		t.Fatalf("a file without a client: %+v", anon)
	}
	// Files that differ but no configured client: the doctor says nothing here (the
	// missing set-up is reported by the check before it).
	if none := settingsFindings(install.Report{Paths: []string{"/a"}, Clients: map[string]string{"/a": "Codex"}}, nil, labelOfKind); len(none) != 0 {
		t.Fatalf("no configured client: %+v", none)
	}
	failed := settingsFindings(install.Report{Configured: []string{"claude"}}, errors.New("lock busy"), labelOfKind)
	if len(failed) != 1 || failed[0].Status != checkProblem || !strings.Contains(failed[0].Name, "lock busy") || failed[0].Fix == "" {
		t.Fatalf("an error = %+v", failed)
	}
}

func doctorSettingsFixture(t *testing.T) (home, state string) {
	t.Helper()
	return doctorFixtureWith(t, map[string]string{"claude": "2.1.0 (Claude Code)", "codex": "codex-cli 0.150.0"})
}

// addFakeClient puts one more fake client command on the fixture's PATH.
func addFakeClient(t *testing.T, home, name, version string) {
	t.Helper()
	fake := filepath.Join(home, "bin", name)
	writeCodexFixture(t, fake, "#!/bin/sh\nprintf '"+version+"\\n'\n")
	if err := os.Chmod(fake, 0700); err != nil {
		t.Fatal(err)
	}
}

// doctorFixtureWith installs Zalki with exactly these fake clients on the PATH.
func doctorFixtureWith(t *testing.T, clients map[string]string) (home, state string) {
	t.Helper()
	home = codexDiagnosticTestDir(t)
	state = filepath.Join(home, "state")
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	for name, out := range clients {
		addFakeClient(t, home, name, out)
	}
	t.Setenv("PATH", filepath.Join(home, "bin"))
	// A bare home: only the clients that are on the PATH are configured (execute would seed every client's folder).
	if _, _, err := executeBare(t, home, "", "install", "--yes", "--state", state); err != nil {
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

func doctorWords(t *testing.T, home, state string) (string, error) {
	t.Helper()
	out, diagnostic, err := execute(t, home, "", "doctor", "--state", state)
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
	if out, err := doctorWords(t, home, state); err != nil || !strings.Contains(out, "[ok]      Claude: Zalki settings are in place") || !strings.Contains(out, "[ok]      Codex: Zalki settings are in place") || !strings.Contains(out, "No problems found.") {
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

// Right after install Codex has not trusted the zalki hooks yet. For a Codex
// version whose hashes Zalki knows, the doctor says so as a problem with the
// one step; the 0.150.0 fixture of the other tests cannot be compared, so it is
// only a note.
func TestDoctorSaysTheCodexHooksAreNotTrustedRightAfterInstall(t *testing.T) {
	home, state := doctorSettingsFixture(t)
	if out, err := doctorWords(t, home, state); err != nil || !strings.Contains(out, "[note]    Codex: hook trust could not be checked") {
		t.Fatalf("an unknown Codex version: %v\n%s", err, out)
	}
	writeCodexFixture(t, filepath.Join(home, "bin", "codex"), "#!/bin/sh\nprintf 'codex-cli 0.167.2\\n'\n")
	if err := os.Chmod(filepath.Join(home, "bin", "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := doctorWords(t, home, state)
	want := "[problem] Codex: its 7 zalki hooks are not trusted yet\n          Next: open /hooks in Codex and trust the zalki hooks, then send a message in each thread\n"
	if !errors.Is(err, errReported) || !strings.Contains(out, want) {
		t.Fatalf("a known Codex version: %v\n%s", err, out)
	}
}

// A client that is on the machine but was never configured is a problem: Zalki
// was installed when only Claude was there; later codex, agent (Cursor) and agy
// (Antigravity) appear. Each is named, with the number of its settings files that
// Zalki would write, and the doctor exits 1. It is never "in place".
func TestAClientThatIsInstalledButNeverConfiguredIsAProblem(t *testing.T) {
	home, state := doctorFixtureWith(t, map[string]string{"claude": "2.1.0 (Claude Code)"})
	if out, err := doctorWords(t, home, state); err != nil || !strings.Contains(out, "No problems found.") || strings.Contains(out, "Codex: Zalki settings are in place") {
		t.Fatalf("only Claude, installed: %v\n%s", err, out)
	}
	addFakeClient(t, home, "codex", "codex-cli 0.150.0")
	addFakeClient(t, home, "agent", "agent 1.0.0")
	addFakeClient(t, home, "agy", "agy 1.5.18")
	before := snapshotTree(t, home)
	out, err := doctorWords(t, home, state)
	if !errors.Is(err, errReported) {
		t.Fatalf("the doctor exit = %v, want the problem exit\n%s", err, out)
	}
	for _, want := range []string{"[problem] Codex: ", "[problem] Cursor: ", "[problem] Antigravity: "} {
		if !strings.Contains(out, want) || !strings.Contains(out, " settings file") {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	for _, client := range []string{"Codex", "Cursor", "Antigravity"} {
		if strings.Contains(out, "[ok]      "+client+": Zalki settings are in place") {
			t.Fatalf("%s was called in place although it was never configured:\n%s", client, out)
		}
	}
	if !strings.Contains(out, "[ok]      Claude: Zalki settings are in place") || !strings.Contains(out, "Next: run zalki upgrade") {
		t.Fatalf("Claude stays in place and the fix is named:\n%s", out)
	}
	after := snapshotTree(t, home)
	for path, sum := range before {
		if after[path] != sum {
			t.Fatalf("the doctor changed %s", path)
		}
	}
	if len(before) != len(after) {
		t.Fatalf("the doctor changed the files: %d before, %d after", len(before), len(after))
	}
}
