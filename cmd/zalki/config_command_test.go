package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func configTestHome(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"ZALKI_CONFIG", "ZALKI_STATE", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "ZALKI_LOG_LEVEL", "ZALKI_LOG_FILE"} {
		t.Setenv(name, "")
	}
	return t.TempDir()
}

func writeConfigFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDefaultReviewFixture(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, ".config", "zalki", "config.json")
	state := filepath.Join(home, ".local", "state", "zalki")
	out, diagnostic, err := execute(t, home, "", "config", "show")
	want := fmt.Sprintf(`{
  "paths": {
    "config_file": {
      "value": %q,
      "source": "default",
      "origin": "home default"
    },
    "state_dir": {
      "value": %q,
      "source": "default",
      "origin": "home default"
    }
  },
  "file_exists": false,
  "settings": {
    "messages": {
      "max_body_bytes": {
        "value": 16384,
        "source": "default",
        "origin": "default messages.max_body_bytes"
      }
    },
    "logging": {
      "level": {
        "value": "info",
        "source": "default",
        "origin": "default logging.level"
      },
      "file": {
        "value": %q,
        "source": "default",
        "origin": "default logging.file (derived from state_dir)"
      }
    },
    "prune": {
      "retired_older_than": {
        "value": "168h0m0s",
        "source": "default",
        "origin": "default prune.retired_older_than"
      }
    }
  }
}
`, path, state, filepath.Join(state, "logs", "zalki.jsonl"))
	if err != nil || diagnostic != "" || out != want {
		t.Fatalf("default review fixture: err=%v stderr=%q stdout=%s", err, diagnostic, out)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("inspector created files: %v, %v", entries, err)
	}
}

func TestConfigSelectionAndSources(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, "selected", "settings.json")
	state := filepath.Join(home, "selected-state")
	log := filepath.Join(home, "unused.log")
	writeConfigFixture(t, path, `{"messages":{"max_body_bytes":1024},"logging":{"level":"warn"},"prune":{"retired_older_than":"48h"}}`)
	t.Setenv("ZALKI_CONFIG", path)
	t.Setenv("ZALKI_STATE", filepath.Join(home, "env-state"))
	t.Setenv("ZALKI_LOG_LEVEL", "debug")
	t.Setenv("ZALKI_LOG_FILE", log)
	t.Setenv("ZALKI_MAX_MESSAGE_BODY_BYTES", "invalid")
	t.Setenv("ZALKI_RETIRED_OLDER_THAN", "invalid")
	out, diagnostic, err := execute(t, home, "", "config", "show", "--state", state, "--log-level", "off")
	var show configShowOutput
	if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &show) != nil {
		t.Fatalf("selected show: %v, %q, %s", err, diagnostic, out)
	}
	checkSelectedConfigShow(t, show, path, state)
	for _, absent := range []string{state, log, filepath.Join(home, "env-state")} {
		if _, err := os.Lstat(absent); !os.IsNotExist(err) {
			t.Fatalf("inspector wrote %s: %v", absent, err)
		}
	}
	other := filepath.Join(home, "override.json")
	writeConfigFixture(t, other, `{}`)
	out, diagnostic, err = execute(t, home, "", "--config", other, "config", "path")
	if err != nil || diagnostic != "" || out != other+"\n" {
		t.Fatalf("config flag did not override env: %v %q %q", err, diagnostic, out)
	}
}

func TestConfigBadFileAndRecovery(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, ".config", "zalki", "config.json")
	writeConfigFixture(t, path, "{\n  \"logging\": {\n    \"lvl\": \"debug\"\n  }\n}\n")
	want := path + " line 3, key \"logging.lvl\": unknown key. Remove it or check the spelling. Allowed keys are file, level"
	out, diagnostic, err := execute(t, home, "", "config", "show", "--log-level", "debug", "--log-file", "-")
	if err == nil || err.Error() != want || out != "" || diagnostic != "" {
		t.Fatalf("bad-file fixture: %v, %q, %q", err, out, diagnostic)
	}
	for _, command := range [][]string{{"config", "path"}, {"version"}, {"--help"}} {
		out, diagnostic, err = execute(t, home, "", command...)
		if err != nil || diagnostic != "" || out == "" {
			t.Fatalf("recovery command %v: %v %q %q", command, err, out, diagnostic)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Fatal("repair commands created state", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	out, diagnostic, err = execute(t, home, "", "config", "path")
	if err != nil || diagnostic != "" || out != path+"\n" {
		t.Fatal("path inspected config permissions", err)
	}
}

func TestConfigGlobalFlagSurface(t *testing.T) {
	home := configTestHome(t)
	root := newRootCommand(home)
	var globals []string
	root.PersistentFlags().VisitAll(func(flag *pflag.Flag) { globals = append(globals, flag.Name) })
	if strings.Join(globals, ",") != "config,log-file,log-level,state" {
		t.Fatal("unexpected global config flags")
	}
	if root.PersistentFlags().FlagUsages() == "" || root.PersistentFlags().Lookup("state") == nil || root.PersistentFlags().Lookup("log-level") == nil || root.PersistentFlags().Lookup("log-file") == nil {
		t.Fatal("existing flags disappeared")
	}
	prune, _, err := root.Find([]string{"prune"})
	if err != nil || prune.Flags().Lookup("retired-older-than") == nil {
		t.Fatal("local prune flag disappeared", err)
	}
	for _, args := range [][]string{{"config", "show", "--max-message-body-bytes", "1024"}, {"config", "show", "--retired-older-than", "1h"}} {
		out, diagnostic, err := execute(t, home, "", args...)
		if err == nil || !strings.Contains(err.Error(), "unknown flag") || out != "" || diagnostic != "" {
			t.Fatalf("removed flag accepted: %v, %v", args, err)
		}
	}
}

func TestConfigServiceOnlyPreRuns(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, "bad.json")
	writeConfigFixture(t, path, `{"unknown":true}`)
	for _, args := range [][]string{
		{"plugins", "read", "fixture", "--session", "codex:00000000-0000-4000-8000-000000000001", "--message", "00000000-0000-4000-8000-000000000002"},
		{"plugins", "inbox-read", "fixture", "--message", "00000000-0000-4000-8000-000000000002"},
		{"plugins", "check", "fixture"},
	} {
		out, diagnostic, err := execute(t, home, "", append([]string{"--config", path}, args...)...)
		if err == nil || !strings.Contains(err.Error(), `key "unknown": unknown key`) || out != "" || diagnostic != "" {
			t.Fatalf("service-only pre-run bypassed config: %v, %v, %q, %q", args, err, out, diagnostic)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Fatal("rejected pre-run created state", err)
	}
}

func TestConfigInvocationSnapshot(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, ".config", "zalki", "config.json")
	writeConfigFixture(t, path, `{}`)
	root := newRootCommand(home)
	doctor, _, err := root.Find([]string{"doctor"})
	if err != nil {
		t.Fatal(err)
	}
	doctor.PreRunE = func(_ *cobra.Command, _ []string) error {
		return os.WriteFile(path, []byte(`{"changed":true}`), 0600)
	}
	var output, diagnostic bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&diagnostic)
	root.SetArgs([]string{"doctor"})
	if err := root.ExecuteContext(t.Context()); err != nil || diagnostic.Len() != 0 || !json.Valid(output.Bytes()) {
		t.Fatalf("service reloaded an invocation snapshot: %v, %s", err, diagnostic.String())
	}
	if _, _, err := execute(t, home, "", "config", "show"); err == nil {
		t.Fatal("a fresh invocation reused the previous snapshot")
	}
}

func TestConfigStateEnvironmentUsedByService(t *testing.T) {
	home := configTestHome(t)
	state := filepath.Join(home, "environment-state")
	t.Setenv("ZALKI_STATE", state)
	out, diagnostic, err := execute(t, home, "", "doctor")
	var report struct {
		State string `json:"state"`
	}
	if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &report) != nil || report.State != state {
		t.Fatalf("service did not receive the resolved state: %v %q %s", err, diagnostic, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Fatal("service created the default state", err)
	}
}

func TestConfigActualCLI(t *testing.T) {
	binary := checkRealBinary(t)
	home := configTestHome(t)
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "zalki", "config.json")
	run := func(args ...string) (string, string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		var output, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &diagnostic
		err := cmd.Run()
		return output.String(), diagnostic.String(), err
	}
	out, diagnostic, err := run("config", "show")
	if err != nil || diagnostic != "" || !json.Valid([]byte(out)) || !strings.Contains(out, path) {
		t.Fatalf("actual default show: %v %q %q", err, out, diagnostic)
	}
	version, diagnostic, err := run("--version")
	if err != nil || diagnostic != "" || version == "" {
		t.Fatalf("actual version baseline: %v %q %q", err, version, diagnostic)
	}
	writeConfigFixture(t, path, "{\n  \"logging\": {\n    \"lvl\": \"debug\"\n  }\n}\n")
	out, diagnostic, err = run("config", "show")
	want := "zalki: " + path + " line 3, key \"logging.lvl\": unknown key. Remove it or check the spelling. Allowed keys are file, level\n"
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || out != "" || diagnostic != want {
		t.Fatalf("actual bad-file show: %v %q %q", err, out, diagnostic)
	}
	checkActualConfigRecovery(t, run, path, version)
}

func TestConfigControlSelectorsHaveOneLineDiagnostics(t *testing.T) {
	binary := checkRealBinary(t)
	for flag, variable := range map[string]string{"config": "ZALKI_CONFIG", "state": "ZALKI_STATE", "log-file": "ZALKI_LOG_FILE"} {
		for _, source := range []string{"flag", "env"} {
			for name, control := range map[string]string{"newline": "\n", "escape": "\x1b[31m"} {
				t.Run(flag+"/"+source+"/"+name, func(t *testing.T) {
					checkControlSelectorDiagnostic(t, binary, flag, variable, source, control)
				})
			}
		}
	}
}

func TestConfigControlLogSelectorDuringVersionRecovery(t *testing.T) {
	binary := checkRealBinary(t)
	for _, source := range []string{"flag", "env"} {
		for name, control := range map[string]string{"newline": "\n", "escape": "\x1b[31m"} {
			t.Run(source+"/"+name, func(t *testing.T) {
				home := configTestHome(t)
				t.Setenv("HOME", home)
				writeConfigFixture(t, filepath.Join(home, ".config", "zalki", "config.json"), "{broken")
				before := cliSnapshot(t, home)
				path := filepath.Join(home, "bad"+control+"log")
				args := []string{"version"}
				if source == "flag" {
					args = append(args, "--log-file", path)
				} else {
					t.Setenv("ZALKI_LOG_FILE", path)
				}
				if out, diagnostic, err := execute(t, home, "", args...); err == nil || out != "" || diagnostic != "" || !strings.Contains(err.Error(), "without control characters (including newlines)") {
					t.Fatal("in-process version recovery bypassed log selector validation")
				}
				cmd := exec.CommandContext(t.Context(), binary, args...)
				var output, diagnostic bytes.Buffer
				cmd.Stdout, cmd.Stderr = &output, &diagnostic
				err := cmd.Run()
				if err == nil || output.Len() != 0 || strings.Count(diagnostic.String(), "\n") != 1 || strings.Contains(diagnostic.String(), "\x1b") || !strings.Contains(diagnostic.String(), "without control characters (including newlines)") || !reflect.DeepEqual(before, cliSnapshot(t, home)) {
					t.Fatalf("version recovery bypassed log selector validation: err=%v stderr=%q", err, diagnostic.String())
				}
			})
		}
	}
}

func checkSelectedConfigShow(t *testing.T, show configShowOutput, path, state string) {
	t.Helper()
	if show.Paths.ConfigFile.Value != path || show.Paths.ConfigFile.Source != "env" || show.Paths.StateDir.Value != state || show.Paths.StateDir.Source != "flag" || !show.FileExists {
		t.Fatalf("selected paths: %+v", show.Paths)
	}
	if show.Settings.Messages.MaxBodyBytes.Value != 1024 || show.Settings.Messages.MaxBodyBytes.Source != "file" || show.Settings.Logging.Level.Source != "flag" || show.Settings.Logging.Level.Value != "off" || show.Settings.Logging.File.Source != "env" || show.Settings.Prune.RetiredOlderThan.Value != "48h0m0s" || show.Settings.Prune.RetiredOlderThan.Source != "file" {
		t.Fatalf("selected settings: %+v", show.Settings)
	}
	if show.Settings.Prune.RetiredOlderThan.Origin != path+"#prune.retired_older_than" {
		t.Fatal("retention origin", show.Settings.Prune.RetiredOlderThan.Origin)
	}
}

func checkActualConfigRecovery(t *testing.T, run func(...string) (string, string, error), path, version string) {
	t.Helper()
	out, diagnostic, err := run("config", "path")
	if err != nil || diagnostic != "" || out != path+"\n" {
		t.Fatalf("actual path recovery: %v %q %q", err, out, diagnostic)
	}
	out, diagnostic, err = run("version")
	if err != nil || diagnostic != "" || out != version {
		t.Fatalf("actual version recovery: %v %q %q", err, out, diagnostic)
	}
}

func checkControlSelectorDiagnostic(t *testing.T, binary, flag, variable, source, control string) {
	t.Helper()
	home := configTestHome(t)
	t.Setenv("HOME", home)
	path := filepath.Join(home, "bad"+control+"path")
	args := []string{"config", "show"}
	if source == "flag" {
		args = append(args, "--"+flag, path)
	} else {
		t.Setenv(variable, path)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	var output, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostic
	err := cmd.Run()
	text := diagnostic.String()
	if err == nil || output.Len() != 0 || strings.Count(text, "\n") != 1 || strings.Contains(text, "\x1b") || !strings.Contains(text, "without control characters (including newlines)") {
		t.Fatalf("unsafe selector diagnostic: stdout=%q stderr=%q err=%v", output.String(), text, err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("selector refusal created local files", err)
	}
}
