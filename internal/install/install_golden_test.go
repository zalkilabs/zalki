package install_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

var updateInstallGolden = flag.Bool("update-install-golden", false, "rewrite the files in testdata/install-golden")

// writeGolden stores the files of a scenario. It runs only with -update-install-golden.
func writeGolden(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// requireGolden compares the files with testdata/install-golden/<scenario>.
func requireGolden(t *testing.T, scenario string, files map[string]string) {
	t.Helper()
	dir := filepath.Join("testdata", "install-golden", scenario)
	if *updateInstallGolden {
		writeGolden(t, dir, files)
		return
	}
	want := map[string]string{}
	if err := filepathWalkFiles(dir, want); err != nil {
		t.Fatal(err)
	}
	for _, name := range sortedNames(files) {
		if got, ok := want[name]; !ok || got != files[name] {
			t.Errorf("%s: %s differs from the golden file\ngot:\n%s\nwant:\n%s", scenario, name, files[name], want[name])
		}
	}
	for _, name := range sortedNames(want) {
		if _, ok := files[name]; !ok {
			t.Errorf("%s: the golden file %s was not produced", scenario, name)
		}
	}
}

func filepathWalkFiles(dir string, into map[string]string) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		into[filepath.ToSlash(rel)] = string(raw)
		return nil
	})
}

// mustManage runs the action for Claude, Codex and Cursor, with the first golden binary.
func mustManage(t *testing.T, home, action string) {
	t.Helper()
	mustManageWith(t, configDeclare, home, action, goldenBinary)
}

// mustManageAll runs the action for every built-in client.
func mustManageAll(t *testing.T, home, action, binary string) {
	t.Helper()
	mustManageWith(t, allClientsDeclare, home, action, binary)
}

// What each client gets on a fresh home, then with the user's own settings,
// after an upgrade to another binary, and after an uninstall.
func TestInstallGoldenFilesOfTheClients(t *testing.T) {
	isolateConfigEnv(t)
	fresh := t.TempDir()
	mustManageAll(t, fresh, "install", goldenBinary)
	requireGolden(t, "fresh-install", snapshotHome(t, fresh))

	home := t.TempDir()
	seedHome(t, home, goldenUserFiles)
	mustManageAll(t, home, "install", goldenBinary)
	requireGolden(t, "user-content-install", snapshotHome(t, home))
	mustManageAll(t, home, "upgrade", goldenNewBinary)
	requireGolden(t, "user-content-upgrade", snapshotHome(t, home))
	mustManageAll(t, home, "uninstall", goldenNewBinary)
	requireGolden(t, "user-content-uninstall", snapshotHome(t, home))
}

// loadRC8Home puts the files an rc.8 installation left into a temporary home.
func loadRC8Home(t *testing.T) string {
	t.Helper()
	files := map[string]string{}
	if err := filepathWalkFiles(filepath.Join("testdata", "rc8-install"), files); err != nil {
		t.Fatal(err)
	}
	delete(files, "README.txt")
	delete(files, "GENERATOR.go.txt")
	home := t.TempDir()
	for name, content := range files {
		files[name] = strings.ReplaceAll(content, goldenHome, home)
	}
	seedHome(t, home, files)
	return home
}

// The installer writes the same bytes that rc.8 wrote.
func TestInstallWritesTheBytesRC8Wrote(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	seedHome(t, home, goldenUserFiles)
	mustManage(t, home, "install")
	want := map[string]string{}
	if err := filepathWalkFiles(filepath.Join("testdata", "rc8-install"), want); err != nil {
		t.Fatal(err)
	}
	delete(want, "README.txt")
	delete(want, "GENERATOR.go.txt")
	got := snapshotHome(t, home)
	for _, name := range sortedNames(want) {
		if got[name] != want[name] {
			t.Errorf("%s differs from what rc.8 wrote\ngot:\n%s\nwant:\n%s", name, got[name], want[name])
		}
	}
	if len(got) != len(want) {
		t.Errorf("files = %v, want the rc.8 files %v", sortedNames(got), sortedNames(want))
	}
}

// An installation made by rc.8 is read by the current installer: an upgrade to
// the same binary changes nothing, an upgrade to another binary and an uninstall
// give the same files as for an installation made now.
func TestUpgradeAndUninstallReadAnRC8Installation(t *testing.T) {
	isolateConfigEnv(t)
	home := loadRC8Home(t)
	before := snapshotHome(t, home)
	mustManage(t, home, "upgrade")
	if after := snapshotHome(t, home); !equalFiles(before, after) {
		t.Fatalf("an upgrade to the same binary changed files: before %v, after %v", sortedNames(before), sortedNames(after))
	}
	mustManageAll(t, home, "upgrade", goldenNewBinary)
	requireGolden(t, "user-content-upgrade", snapshotHome(t, home))
	mustManageAll(t, home, "uninstall", goldenNewBinary)
	requireGolden(t, "user-content-uninstall", snapshotHome(t, home))
}

func equalFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, content := range a {
		if b[name] != content {
			return false
		}
	}
	return true
}

// An installation made by an earlier version keeps working when a client is gone: the
// upgrade leaves that client's files and record entries alone and does not make its
// directory again.
func TestUpgradeOfAnEarlierInstallationKeepsAClientThatIsGone(t *testing.T) {
	isolateConfigEnv(t)
	home := loadRC8Home(t)
	if err := os.RemoveAll(filepath.Join(home, ".cursor")); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(filepath.Join(home, "state", "installation.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := goldenRequest(home, "upgrade", goldenBinary)
	request.FindExecutable = func(string) bool { return false }
	report, err := install.Manage(clientAwareDeclare, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Configured, []string{"claude", "codex"}) || len(report.Paths) != 0 {
		t.Fatalf("configured %v, edits %v; want claude and codex configured and nothing to change", report.Configured, report.Paths)
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor")); !os.IsNotExist(err) {
		t.Fatalf("the directory of a client that is gone was made again (%v)", err)
	}
	if after, err := os.ReadFile(filepath.Join(home, "state", "installation.json")); err != nil || string(after) != string(record) {
		t.Fatalf("the record changed (%v)", err)
	}
}

// An installation of three clients, upgraded by an installer that knows Antigravity,
// with the Antigravity directory present: Antigravity is added, and the files and the
// record entries of the other three do not change.
func TestUpgradeOfAnInstallationOfThreeClientsAddsAntigravity(t *testing.T) {
	isolateConfigEnv(t)
	home := loadRC8Home(t)
	if err := os.MkdirAll(filepath.Join(home, ".gemini", "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshotHome(t, home)
	if _, err := install.Manage(clientAwareDeclare, goldenRequest(home, "upgrade", goldenBinary)); err != nil {
		t.Fatal(err)
	}
	after := snapshotHome(t, home)
	for name, content := range before {
		if name != "state/installation.json" && after[name] != content {
			t.Errorf("%s changed", name)
		}
	}
	for _, name := range []string{".gemini/config/hooks.json", ".gemini/config/mcp_config.json"} {
		if !strings.Contains(after[name], "zalki") {
			t.Errorf("%s was not written for Antigravity", name)
		}
	}
	was, now := readRecordParts(t, before["state/installation.json"]), readRecordParts(t, after["state/installation.json"])
	for key, target := range was.Targets {
		if now.Targets[key] != target {
			t.Errorf("target %s changed to %q", key, now.Targets[key])
		}
	}
	requireSameRaw(t, "patch", was.Patches, now.Patches)
	requireSameRaw(t, "fragment", was.Fragments, now.Fragments)
	if len(now.Targets) != len(was.Targets)+2 {
		t.Errorf("record has %d targets, want %d", len(now.Targets), len(was.Targets)+2)
	}
}

type recordParts struct {
	Targets   map[string]string          `json:"targets"`
	Patches   map[string]json.RawMessage `json:"patches"`
	Fragments map[string]json.RawMessage `json:"fragments"`
}

func readRecordParts(t *testing.T, raw string) recordParts {
	t.Helper()
	var parts recordParts
	if err := json.Unmarshal([]byte(raw), &parts); err != nil {
		t.Fatal(err)
	}
	return parts
}

// requireSameRaw checks that every entry that was recorded is recorded the same way.
func requireSameRaw(t *testing.T, kind string, was, now map[string]json.RawMessage) {
	t.Helper()
	for key, raw := range was {
		if string(now[key]) != string(raw) {
			t.Errorf("%s %s changed", kind, key)
		}
	}
}
