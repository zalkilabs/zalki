package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReservedTargetsResolveMissingPathsAndState(t *testing.T) {
	home := t.TempDir()
	realDir := filepath.Join(home, "real")
	alias := filepath.Join(home, "alias")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	file := ConfigFile{Draft: "fixture.json"}
	for _, test := range []struct {
		name, target, config, state, selector string
	}{
		{"missing config through parent alias", filepath.Join(realDir, "future", "client.json"), filepath.Join(alias, "future", "client.json"), filepath.Join(home, "state"), "--config"},
		{"state directory", filepath.Join(alias, "future"), "", filepath.Join(realDir, "future"), "--state"},
		{"installation manifest", filepath.Join(alias, "future", "installation.json"), "", filepath.Join(realDir, "future"), "--state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateReservedTarget(test.target, file, Request{ConfigFile: test.config, StateDir: test.state, HomeDir: home})
			if err == nil || !strings.Contains(err.Error(), test.selector+" points at a file Zalki edits") {
				t.Fatal("missing reserved path was not rejected", err)
			}
		})
	}
	if err := validateReservedTarget(filepath.Join(realDir, "future", "client.json"), file, Request{ConfigFile: filepath.Join(alias, "another.json"), StateDir: filepath.Join(home, "state"), HomeDir: home}); err != nil {
		t.Fatal("distinct future target was rejected", err)
	}
	if err := validateReservedTarget(filepath.Join(home, "client.json"), file, Request{ConfigFile: filepath.Join(alias, "future", "installation.json"), StateDir: filepath.Join(realDir, "future"), HomeDir: home}); err == nil || !strings.Contains(err.Error(), "--config points at Zalki state") {
		t.Fatal("configuration could be overwritten by the installation manifest", err)
	}
}
