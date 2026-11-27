package install_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// The golden tests run the installer on a temporary home and compare every file
// it leaves behind, byte for byte, with files in testdata. The home path is
// written as <HOME> in the files. Backups and the lock file are not compared.

const (
	goldenBinary    = "/opt/zalki tools/zalki"
	goldenNewBinary = "/opt/new tools/zalki"
	goldenHome      = "<HOME>"
)

// goldenUserFiles are settings files a user already has: their own keys, hooks,
// servers, a float and a big integer that must keep their spelling.
var goldenUserFiles = map[string]string{
	".claude/settings.json": `{"theme":"mine","ratio":1.50,"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo mine"}]}]}}`,
	".claude.json":          `{"projects":{},"count":12345678901234567890,"mcpServers":{"mine":{"command":"x"}}}`,
	".cursor/hooks.json":    `{"version":1,"hooks":{"stop":[{"command":"echo mine"}]}}`,
	".cursor/mcp.json":      `{"mcpServers":{"mine":{"command":"x","args":["a"]}}}`,
	".codex/hooks.json":     `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo mine"}]}]}}`,
	".codex/config.toml":    "model = \"x\"\n\n[mcp_servers.mine]\ncommand = \"x\"\n",
}

func seedHome(t *testing.T, home string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(home, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func goldenRequest(home, action, binary string) install.Request {
	return install.Request{Action: action, Binary: binary, StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true}
}

// snapshotHome returns every file under home with the home path replaced by <HOME>.
func snapshotHome(t *testing.T, home string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name := entry.Name()
		if strings.Contains(name, ".zalki-backup-") || name == ".zalki-install.lock" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = strings.ReplaceAll(string(raw), home, goldenHome)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sortedNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
