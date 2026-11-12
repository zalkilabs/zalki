package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUpgradeFreshDryRun(t *testing.T) {
	home := configTestHome(t)
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, "")
	}
	f := upgradeFixture{}
	f.locate(t, "/fixture/prefix/bin/zalki", "/fixture/prefix/Caskroom/zalki/1/zalki")
	for _, extra := range [][]string{nil, {"--config-only"}} {
		before := bootstrapSnapshot(t, home)
		out, diagnostic, err := execute(t, home, "", append([]string{"upgrade", "--dry-run"}, extra...)...)
		var report upgradePreview
		if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &report) != nil || !report.DryRun || report.Action != "upgrade" || len(report.Paths) != 6 {
			t.Fatal("fresh dry run", out, diagnostic, err)
		}
		if !reflect.DeepEqual(before, bootstrapSnapshot(t, home)) {
			t.Fatal("fresh preview wrote files")
		}
		want := brewUpgradeHint
		if len(extra) > 0 {
			want = "skipped (--config-only)"
		}
		if report.BinaryUpgrade != want {
			t.Fatal("wrong upgrade step", report)
		}
	}
}

func TestUpgradeDamagedRecordIsNotFresh(t *testing.T) {
	for _, record := range []string{"installation.json", "installation.pending.json"} {
		for _, raw := range []string{"{", "{}", "[]", "null", `{"schema":2}`} {
			t.Run(record+"/"+raw, func(t *testing.T) {
				f := newUpgradeFixture(t, "", "echo 'Upgrade successful'\n")
				path := filepath.Join(f.state, record)
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				before := bootstrapSnapshot(t, f.home)
				out, _, err := execute(t, f.home, "", "upgrade", "--state", f.state, "--log-level=off")
				if err == nil || out != "" || f.logged(t, f.brewLog) != "" || f.logged(t, f.childLog) != "" {
					t.Fatal("damaged ownership reached Brew", out, err)
				}
				if !strings.Contains(err.Error(), "installation record") || !reflect.DeepEqual(before, bootstrapSnapshot(t, f.home)) {
					t.Fatal("damaged record not preserved", err)
				}
			})
		}
	}
}

func TestUpgradeFreshConfigOnly(t *testing.T) {
	home := configTestHome(t)
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, "")
	}
	out, diagnostic, err := execute(t, home, "", "upgrade", "--config-only")
	if err != nil || diagnostic != "" || out != "Configuration refreshed\n" {
		t.Fatal("fresh config-only", out, diagnostic, err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".local", "state", "zalki", "installation.json"))
	if err != nil || !json.Valid(raw) {
		t.Fatal("fresh config-only missing ownership", err)
	}
}

func TestUpgradeWithoutInstallationRecordCLI(t *testing.T) {
	binary := checkRealBinary(t)
	home := configTestHome(t)
	t.Setenv("HOME", home)
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, "")
	}
	prefix := filepath.Join(t.TempDir(), "prefix")
	cask := filepath.Join(prefix, "Caskroom", "zalki", "fixture", "zalki")
	stable := filepath.Join(prefix, "bin", "zalki")
	for _, dir := range []string{filepath.Dir(cask), filepath.Dir(stable)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cask, data, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cask, stable); err != nil {
		t.Fatal(err)
	}
	brewLog := filepath.Join(prefix, "brew.log")
	script := "#!/bin/sh\necho \"$@\" > '" + brewLog + "'\necho noisy brew progress\n"
	if err := os.WriteFile(filepath.Join(prefix, "bin", "brew"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	path, state := filepath.Join(home, "selected.json"), filepath.Join(home, "state")
	out, diagnostic, err := runEditCLI(t, stable, "--config", path, "--state", state, "upgrade")
	if err != nil || diagnostic != "" || out != "Upgrade successful\n" {
		t.Fatal("real fresh upgrade", out, diagnostic, err)
	}
	brew, _ := os.ReadFile(brewLog)
	if string(brew) != "upgrade --cask zalkilabs/tap/zalki\n" {
		t.Fatal("real prefix brew did not run", string(brew))
	}
	record, err := os.ReadFile(filepath.Join(state, "installation.json"))
	if err != nil || !bytes.Contains(record, []byte(stable)) {
		t.Fatal("actual finish child did not install pinned binary", string(record), err)
	}
}
