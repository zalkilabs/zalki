package install_test

import (
	"flag"
	"os"
	"path/filepath"
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

func mustManage(t *testing.T, home, action, binary string) {
	t.Helper()
	if _, err := install.Manage(configDeclare, goldenRequest(home, action, binary)); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

// What the three clients get on a fresh home, then with the user's own settings,
// after an upgrade to another binary, and after an uninstall.
func TestInstallGoldenFilesOfTheThreeClients(t *testing.T) {
	isolateConfigEnv(t)
	fresh := t.TempDir()
	mustManage(t, fresh, "install", goldenBinary)
	requireGolden(t, "fresh-install", snapshotHome(t, fresh))

	home := t.TempDir()
	seedHome(t, home, goldenUserFiles)
	mustManage(t, home, "install", goldenBinary)
	requireGolden(t, "user-content-install", snapshotHome(t, home))
	mustManage(t, home, "upgrade", goldenNewBinary)
	requireGolden(t, "user-content-upgrade", snapshotHome(t, home))
	mustManage(t, home, "uninstall", goldenNewBinary)
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
	mustManage(t, home, "install", goldenBinary)
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
	mustManage(t, home, "upgrade", goldenBinary)
	if after := snapshotHome(t, home); !equalFiles(before, after) {
		t.Fatalf("an upgrade to the same binary changed files: before %v, after %v", sortedNames(before), sortedNames(after))
	}
	mustManage(t, home, "upgrade", goldenNewBinary)
	requireGolden(t, "user-content-upgrade", snapshotHome(t, home))
	mustManage(t, home, "uninstall", goldenNewBinary)
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
