package bus

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

func TestAdapterDefinesItsOwnConfigurationOverride(t *testing.T) {
	home, profile := t.TempDir(), t.TempDir()
	t.Setenv("FOURTH_CONFIG_DIR", profile)
	adapter := configFixtureAdapter{
		fixtureAdapter: fixtureAdapter{kind: "fourth", inbox: InboxNative},
		files:          []install.ConfigFile{{Path: ".fourth/mcp.json", ConfigRootEnv: "FOURTH_CONFIG_DIR", Draft: "fourth-mcp.json", JSON: install.MCPConfig("/fixture/zalki", nil, false)}},
	}
	registry, err := NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "state")
	if _, err := install.Manage(registry.Configuration, install.Request{Action: "install", Binary: "/fixture/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(profile, "mcp.json")
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOURTH_CONFIG_DIR", "")
	if _, err := install.Manage(registry.Configuration, install.Request{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if bytes.Contains(data, []byte("/fixture/zalki")) {
		t.Fatal("fourth adapter could not uninstall from its recorded profile")
	}
}

func clearConfigOverrides(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
}

func installationSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
