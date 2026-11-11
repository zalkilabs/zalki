package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// These tests drive the four configuration commands (install, upgrade,
// uninstall and setup) through the command line and check printed output and
// files on disk. They use only the existing execute helper, temporary homes
// and cleared configuration variables. They always pass --out, because the
// default draft directory is relative to the working directory.

func cliEnv(t *testing.T) (home, state string) {
	t.Helper()
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	home = t.TempDir()
	return home, filepath.Join(home, "state")
}

// configTargets are the six files the built-in clients configure, in the order
// the commands report them.
func configTargets(home string) []string {
	return []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".codex", "hooks.json"),
		filepath.Join(home, ".cursor", "hooks.json"),
		filepath.Join(home, ".cursor", "mcp.json"),
	}
}

const installAppliedNote = "Existing settings were preserved and backed up. Review/trust integrations in each configured client, then launch normally. Check doctor for adapter and listener requirements.\n"

// cliSnapshot reads every file under dir except the command logs, which change
// on every command.
func cliSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if entry.IsDir() {
			if rel == filepath.Join("state", "logs") {
				return fs.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(path)
		files[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func readRecord(t *testing.T, state string) (record struct {
	Schema  int               `json:"schema"`
	Home    string            `json:"home"`
	Binary  string            `json:"binary"`
	State   string            `json:"state"`
	Targets map[string]string `json:"targets"`
}) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestInstallCommandDraftsFirstThenApplies(t *testing.T) {
	home, state := cliEnv(t)
	drafts := filepath.Join(home, "drafts")

	out, diagnostic, err := execute(t, home, "", "install", "--state", state, "--out", drafts)
	want := defaultConfigLine(home) + "Configuration drafts written to " + drafts + "\nTo apply after review: zalki install --apply\n"
	if err != nil || diagnostic != "" || out != want {
		t.Fatalf("draft run: out %q diagnostic %q err %v; want %q", out, diagnostic, err, want)
	}
	names := []string{}
	entries, _ := os.ReadDir(drafts)
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if wantNames := []string{"claude-hooks.json", "claude-mcp.json", "codex-hooks.json", "codex-mcp.toml", "cursor-hooks.json", "cursor-mcp.json", "targets.json"}; !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("drafts = %v, want %v", names, wantNames)
	}
	for _, target := range configTargets(home) {
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("drafting wrote %s: %v", target, err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "installation.json")); !os.IsNotExist(err) {
		t.Fatalf("drafting wrote an installation record: %v", err)
	}

	out, diagnostic, err = execute(t, home, "", "install", "--state", state, "--apply")
	var updated strings.Builder
	for _, target := range configTargets(home) {
		updated.WriteString("Updated: " + target + "\n")
	}
	if err != nil || diagnostic != "" || out != defaultConfigLine(home)+updated.String()+installAppliedNote {
		t.Fatalf("apply run: out %q diagnostic %q err %v", out, diagnostic, err)
	}
	binary, err := installationBinary()
	if err != nil {
		t.Fatal(err)
	}
	record := readRecord(t, state)
	if record.Schema != 3 || record.Home != home || record.State != state || record.Binary != binary || len(record.Targets) != 6 {
		t.Fatalf("record = %+v, want schema 3 home %s state %s binary %s and six targets", record, home, state, binary)
	}

	// The same command again has nothing to update, and changes no file.
	before := cliSnapshot(t, home)
	out, _, err = execute(t, home, "", "install", "--state", state, "--apply")
	if err != nil || out != defaultConfigLine(home)+installAppliedNote || !reflect.DeepEqual(before, cliSnapshot(t, home)) {
		t.Fatalf("repeat apply: out %q err %v, files changed: %v", out, err, !reflect.DeepEqual(before, cliSnapshot(t, home)))
	}
}

func TestUpgradeConfigOnlyMovesHooksToTheNewBinaryAndKeepsUserSettings(t *testing.T) {
	home, state := cliEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"theme":"mine"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, home, "", "install", "--state", state, "--apply"); err != nil {
		t.Fatal(err)
	}
	oldBinary, err := installationBinary()
	if err != nil {
		t.Fatal(err)
	}
	const newBinary = "/opt/new tools/zalki"
	old := locateInstallation
	locateInstallation = func() (binaryLocation, error) { return binaryLocation{Binary: newBinary, Resolved: newBinary}, nil }
	t.Cleanup(func() { locateInstallation = old })

	// A dry run reports the six files and writes nothing.
	before := cliSnapshot(t, home)
	out, diagnostic, err := execute(t, home, "", "upgrade", "--dry-run", "--state", state)
	var report struct {
		Action string   `json:"action"`
		DryRun bool     `json:"dry_run"`
		Paths  []string `json:"paths"`
	}
	if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &report) != nil ||
		report.Action != "upgrade" || !report.DryRun || !reflect.DeepEqual(report.Paths, configTargets(home)) {
		t.Fatalf("dry run: out %q diagnostic %q err %v", out, diagnostic, err)
	}
	if !reflect.DeepEqual(before, cliSnapshot(t, home)) {
		t.Fatal("dry run changed files")
	}

	out, diagnostic, err = execute(t, home, "", "upgrade", "--config-only", "--state", state)
	if err != nil || diagnostic != "" || out != "Configuration refreshed\n" {
		t.Fatalf("config-only: out %q diagnostic %q err %v", out, diagnostic, err)
	}
	if record := readRecord(t, state); record.Binary != newBinary {
		t.Fatalf("record binary = %q, want %q", record.Binary, newBinary)
	}
	quoted := "'" + newBinary + "'"
	for _, target := range configTargets(home) {
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), oldBinary) {
			t.Errorf("%s still names the old binary", target)
		}
		if !strings.Contains(string(body), newBinary) {
			t.Errorf("%s does not name the new binary", target)
		}
		if strings.Contains(filepath.Base(target), "hooks") || filepath.Base(target) == "settings.json" {
			if !strings.Contains(string(body), quoted) {
				t.Errorf("%s: hook commands do not use the quoted new binary %s", target, quoted)
			}
		}
	}
	if body, _ := os.ReadFile(settings); !strings.Contains(string(body), `"theme": "mine"`) {
		t.Errorf("upgrade lost the user's setting: %s", body)
	}
	if backups, _ := filepath.Glob(settings + ".zalki-backup-*"); len(backups) != 2 {
		t.Errorf("settings backups = %v, want one from install and one from upgrade", backups)
	}

	// Refreshing again is a no-op.
	settled := cliSnapshot(t, home)
	if out, _, err := execute(t, home, "", "upgrade", "--config-only", "--state", state); err != nil || out != "Configuration refreshed\n" || !reflect.DeepEqual(settled, cliSnapshot(t, home)) {
		t.Fatalf("second refresh: out %q err %v", out, err)
	}
}

func TestUninstallCommandReportsOwnedFilesAndHandlesNoRecord(t *testing.T) {
	home, state := cliEnv(t)

	// Without a record there are no owned targets to remove or create.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"uninstall", "--state", state}, `{"action":"uninstall","dry_run":true,"paths":[]}` + "\n"},
		{[]string{"uninstall", "--state", state, "--apply"}, `{"action":"uninstall","dry_run":false,"paths":[]}` + "\n"},
	} {
		out, diagnostic, err := execute(t, home, "", tc.args...)
		if err != nil || diagnostic != "" || out != tc.want {
			t.Fatalf("%v: out %q diagnostic %q err %v; want %q", tc.args, out, diagnostic, err, tc.want)
		}
	}
	for _, target := range configTargets(home) {
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("uninstall without a record created %s", target)
		}
	}

	if _, _, err := execute(t, home, "", "install", "--state", state, "--apply"); err != nil {
		t.Fatal(err)
	}
	before := cliSnapshot(t, home)
	out, _, err := execute(t, home, "", "uninstall", "--state", state)
	wantPaths, _ := json.Marshal(configTargets(home))
	if err != nil || out != `{"action":"uninstall","dry_run":true,"paths":`+string(wantPaths)+"}\n" || !reflect.DeepEqual(before, cliSnapshot(t, home)) {
		t.Fatalf("preview: out %q err %v", out, err)
	}
	out, _, err = execute(t, home, "", "uninstall", "--state", state, "--apply")
	if err != nil || out != `{"action":"uninstall","dry_run":false,"paths":`+string(wantPaths)+"}\n" {
		t.Fatalf("apply: out %q err %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(state, "installation.json")); !os.IsNotExist(err) {
		t.Fatalf("the record survived uninstall: %v", err)
	}
	binary, _ := installationBinary()
	for _, target := range configTargets(home) {
		body, err := os.ReadFile(target)
		if err != nil && !os.IsNotExist(err) {
			t.Errorf("cannot inspect %s after uninstall: %v", target, err)
		}
		if strings.Contains(string(body), binary) {
			t.Errorf("%s still holds the zalki entry", target)
		}
	}
}

func TestRelativeConfigDirectoryIsRefusedWithoutChangingConfiguration(t *testing.T) {
	home, state := cliEnv(t)
	if _, _, err := execute(t, home, "", "install", "--state", state, "--apply"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "relative/profile")
	const message = "CODEX_HOME must be an absolute configuration directory"
	before := cliSnapshot(t, home)
	for _, args := range [][]string{
		{"install", "--state", state, "--out", filepath.Join(home, "drafts")},
		{"setup", "--state", state, "--out", filepath.Join(home, "drafts")},
		{"install", "--state", state, "--apply"},
		{"upgrade", "--config-only", "--state", state},
		{"upgrade", "--dry-run", "--state", state},
	} {
		out, _, err := execute(t, home, "", args...)
		if err == nil || err.Error() != message || out != "" {
			t.Errorf("%v: out %q err %v; want error %q", args, out, err, message)
		}
	}
	if !reflect.DeepEqual(before, cliSnapshot(t, home)) {
		t.Error("a refused command wrote files")
	}
	// Uninstall uses the targets recorded at install time, so it still works.
	if _, _, err := execute(t, home, "", "uninstall", "--state", state, "--apply"); err != nil {
		t.Errorf("uninstall with a relative CODEX_HOME: %v", err)
	}
}
