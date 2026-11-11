package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"
)

func TestPathSelectorsRejectNonGraphicCharacters(t *testing.T) {
	bad := map[string]string{"newline": "\n", "escape": "\x1b[31m", "tab": "\t", "nul": "\x00", "delete": "\x7f", "c1": "\u0085", "line_separator": "\u2028", "paragraph_separator": "\u2029", "bidi": "\u202e", "zero_width": "\u200b", "private_use": "\ue000", "invalid_utf8": "\xff"}
	for flag, variable := range map[string]string{"config": "ZALKI_CONFIG", "state": "ZALKI_STATE", "log-file": "ZALKI_LOG_FILE"} {
		for _, source := range []string{"flag", "env"} {
			for name, control := range bad {
				t.Run(flag+"/"+source+"/"+name, func(t *testing.T) {
					env := map[string]string{}
					input := fixtureInput(t, env)
					value := filepath.Join(input.Home, "bad"+control+"path")
					if source == "flag" {
						input.Flags[flag] = value
					} else {
						env[variable] = value
					}
					_, err := Load(input)
					if err == nil || !strings.Contains(err.Error(), "Use a nonempty path without control characters (including newlines)") || strings.IndexFunc(err.Error(), func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
						t.Fatalf("selector diagnostic is missing or contains non-graphic text: %q", err)
					}
				})
			}
		}
	}
}

func TestPathSelectorsPreserveGraphicSpaces(t *testing.T) {
	for _, flag := range []string{"config", "state", "log-file"} {
		input := fixtureInput(t, nil)
		input.Flags[flag] = filepath.Join(input.Home, "folder\u00a0with\u2007graphic\u3000spaces")
		if _, err := Load(input); err != nil {
			t.Fatalf("graphic spaces rejected for %s: %v", flag, err)
		}
	}
	input := fixtureInput(t, nil)
	input.Flags["config"] = "relative.json"
	input.Cwd = filepath.Join(input.Home, "bad\nparent")
	if _, err := Load(input); err == nil || strings.Contains(err.Error(), "bad\nparent") {
		t.Fatal("relative selector did not validate its resolved cwd")
	}
}

func TestFileLogPathRejectsControlCharacters(t *testing.T) {
	for _, control := range []string{"\n", "\x1b", "\u202e"} {
		input := fixtureInput(t, nil)
		raw, err := json.Marshal(map[string]any{"logging": map[string]string{"file": "bad" + control + "path"}})
		if err != nil {
			t.Fatal(err)
		}
		fixtureFile(t, input, string(raw))
		if _, err := Load(input); err == nil || !strings.Contains(err.Error(), "invalid log path") || strings.IndexFunc(err.Error(), func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
			t.Fatalf("file log-path diagnostic: %q", err)
		}
	}
}

func fixtureInput(t *testing.T, env map[string]string) Inputs {
	t.Helper()
	home := t.TempDir()
	return Inputs{Home: home, Cwd: home, GOOS: "linux", Flags: map[string]string{},
		LookupEnv: func(key string) (string, bool) { value, ok := env[key]; return value, ok }}
}

func fixtureFile(t *testing.T, input Inputs, raw string) string {
	t.Helper()
	paths, err := ResolvePaths(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile.Value, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return paths.ConfigFile.Value
}

func TestPathPrecedenceAndPlatforms(t *testing.T) {
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			input := fixtureInput(t, nil)
			input.GOOS = platform
			paths, err := ResolvePaths(input)
			if err != nil || paths.ConfigFile.Value != filepath.Join(input.Home, ".config", "zalki", "config.json") || paths.StateDir.Value != filepath.Join(input.Home, ".local", "state", "zalki") {
				t.Fatalf("default paths: %+v, %v", paths, err)
			}
			if paths.ConfigFile.Source != "default" || paths.StateDir.Source != "default" || paths.DefaultLogFile != filepath.Join(paths.StateDir.Value, "logs", "zalki.jsonl") {
				t.Fatalf("default sources/log path: %+v", paths)
			}
		})
	}
	for _, test := range []pathChoiceCase{
		{"config", "XDG_CONFIG_HOME", "config", filepath.Join("zalki", "config.json"), true},
		{"state", "XDG_STATE_HOME", "state", "zalki", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkPathChoicePrecedence(t, test)
		})
	}
}

func TestPathFailuresAndInvalidXDG(t *testing.T) {
	for _, value := range []string{"", "relative"} {
		input := fixtureInput(t, map[string]string{"XDG_CONFIG_HOME": value, "XDG_STATE_HOME": value})
		paths, err := ResolvePaths(input)
		if err != nil || paths.ConfigFile.Source != "default" || paths.StateDir.Source != "default" {
			t.Fatalf("invalid XDG %q: %+v, %v", value, paths, err)
		}
	}
	for _, flag := range []string{"config", "state"} {
		for _, value := range []string{"", "bad\x00path"} {
			input := fixtureInput(t, nil)
			input.Flags[flag] = value
			if _, err := ResolvePaths(input); err == nil || !strings.Contains(err.Error(), "Use a nonempty path without control characters (including newlines)") {
				t.Fatalf("invalid --%s was accepted: %v", flag, err)
			}
		}
	}
	input := fixtureInput(t, nil)
	input.Flags["state"] = "relative"
	input.Cwd = "relative-cwd"
	if _, err := ResolvePaths(input); err == nil || !strings.Contains(err.Error(), "valid absolute home/cwd") {
		t.Fatalf("invalid cwd: %v", err)
	}
	input.Home = "relative-home"
	input.Flags = nil
	if _, err := ResolvePaths(input); err == nil {
		t.Fatal("relative home was accepted")
	}
	input.GOOS = "windows"
	if _, err := ResolvePaths(input); err == nil {
		t.Fatal("Windows was reported as implemented")
	}
}

func TestLoadPrecedenceAndSources(t *testing.T) {
	for _, test := range []struct{ name, env, flag, source, want string }{
		{"file", "", "", "file", "warn"},
		{"env", "debug", "", "env", "debug"},
		{"flag", "debug", "off", "flag", "off"},
		{"explicit_default", "debug", "info", "flag", "info"},
		{"masked_bad_env", "do-not-echo-this-value", "off", "flag", "off"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := fixtureInput(t, map[string]string{"ZALKI_LOG_LEVEL": test.env})
			fixtureFile(t, input, `{"logging":{"level":"warn"}}`)
			if test.flag != "" {
				input.Flags["log-level"] = test.flag
			}
			snapshot, err := Load(input)
			value := snapshot.Settings.LogLevel
			if err != nil || value.Value != test.want || value.Source != test.source || !snapshot.FileExists {
				t.Fatalf("logging precedence: %+v, %v", value, err)
			}
			origins := map[string]string{"flag": "--log-level", "env": "ZALKI_LOG_LEVEL", "file": snapshot.Paths.ConfigFile.Value + "#logging.level"}
			if value.Origin != origins[test.source] {
				t.Fatalf("logging origin %q", value.Origin)
			}
		})
	}
}

func TestFileOnlySettings(t *testing.T) {
	input := fixtureInput(t, map[string]string{"ZALKI_MAX_MESSAGE_BODY_BYTES": "invalid", "ZALKI_RETIRED_OLDER_THAN": "invalid"})
	input.Flags["max-message-body-bytes"] = "invalid"
	input.Flags["retired-older-than"] = "invalid"
	snapshot, err := Load(input)
	if err != nil || snapshot.Settings.MaxBodyBytes.Value != 16384 || snapshot.Settings.RetiredOlderThan.Value != 168*time.Hour {
		t.Fatalf("file-only defaults: %+v, %v", snapshot, err)
	}
	fixtureFile(t, input, `{"messages":{"max_body_bytes":256},"prune":{"retired_older_than":"0"}}`)
	snapshot, err = Load(input)
	if err != nil || snapshot.Settings.MaxBodyBytes.Value != 256 || snapshot.Settings.MaxBodyBytes.Source != "file" || snapshot.Settings.RetiredOlderThan.Value != 0 || snapshot.Settings.RetiredOlderThan.Source != "file" {
		t.Fatalf("file-only values: %+v, %v", snapshot, err)
	}
}

func TestDefaultsMissingFileAndNoWrites(t *testing.T) {
	input := fixtureInput(t, nil)
	input.Flags["config"] = filepath.Join(input.Home, "missing", "explicit.json")
	snapshot, err := Load(input)
	if err != nil || snapshot.FileExists || snapshot.Settings.MaxBodyBytes.Value != 16384 || snapshot.Settings.LogLevel.Value != "info" || snapshot.Settings.RetiredOlderThan.Value != 168*time.Hour {
		t.Fatalf("missing file defaults: %+v, %v", snapshot, err)
	}
	if snapshot.Settings.MaxBodyBytes.Source != "default" || snapshot.Settings.LogFile.Source != "default" || !strings.Contains(snapshot.Settings.LogFile.Origin, "derived from state_dir") {
		t.Fatalf("default sources: %+v", snapshot.Settings)
	}
	entries, err := os.ReadDir(input.Home)
	if err != nil || len(entries) != 0 {
		t.Fatalf("loading missing config created files: %v, %v", entries, err)
	}
}

func TestLogFileOriginsAndSnapshot(t *testing.T) {
	env := map[string]string{}
	input := fixtureInput(t, env)
	fixtureFile(t, input, `{"logging":{"file":"relative.log"},"prune":{"retired_older_than":"0"}}`)
	snapshot, err := Load(input)
	if err != nil || snapshot.Settings.LogFile.Value != filepath.Join(snapshot.Paths.ConfigDir, "relative.log") || snapshot.Settings.LogFile.Source != "file" || snapshot.Settings.RetiredOlderThan.Value != 0 {
		t.Fatalf("file-relative log: %+v, %v", snapshot, err)
	}
	for _, value := range []string{"", "-"} {
		input.Flags["log-file"] = value
		selected, err := Load(input)
		want := value
		if want == "" {
			want = selected.Paths.DefaultLogFile
		}
		if err != nil || selected.Settings.LogFile.Value != want || selected.Settings.LogFile.Source != "flag" || selected.Settings.LogFileFromState != (value == "") {
			t.Fatalf("explicit log selection: %+v, %v", selected.Settings.LogFile, err)
		}
	}
	counts := map[string]int{}
	input.LookupEnv = func(key string) (string, bool) { counts[key]++; value, ok := env[key]; return value, ok }
	if _, err := Load(input); err != nil {
		t.Fatal(err)
	}
	for key, count := range counts {
		if count != 1 {
			t.Fatalf("environment %s read %d times", key, count)
		}
	}
	env["ZALKI_LOG_FILE"] = "later.log"
	input.Flags["log-file"] = "later-flag.log"
	if snapshot.Settings.LogFile.Value != filepath.Join(snapshot.Paths.ConfigDir, "relative.log") {
		t.Fatal("snapshot changed after inputs changed")
	}
}

func TestLoggingSettingPrecedence(t *testing.T) {
	for _, test := range []loggingChoiceCase{
		{"level", "ZALKI_LOG_LEVEL", "log-level", "warn", "off", func(s Settings) Value[string] { return s.LogLevel }},
		{"file", "ZALKI_LOG_FILE", "log-file", "/env.log", "-", func(s Settings) Value[string] { return s.LogFile }},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkLoggingSettingPrecedence(t, test)
		})
	}
	input := fixtureInput(t, nil)
	paths, err := ResolvePaths(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Flags["log-file"] = paths.DefaultLogFile
	snapshot, err := Load(input)
	if err != nil || snapshot.Settings.LogFileFromState {
		t.Fatalf("explicit log path incorrectly became lazy: %+v, %v", snapshot.Settings, err)
	}
}

func TestStrictConfigDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, raw, located, repair string
	}{
		{"unknown", "{\n\n\"logging\": {\n\"x\":1}}", "line 4, key \"logging.x\": unknown key", "Allowed keys are file, level"},
		{"unknown_root", `{"x":1}`, `key "x": unknown key`, "Allowed keys are logging, messages, prune"},
		{"duplicate", "{\n\"logging\": {\n\"level\":\"info\",\n\"level\":\"debug\"}}", "line 4, key \"logging.level\": duplicate key", "Remove the duplicate key"},
		{"escaped_duplicate", `{"messages":{"max_body_bytes":1,"max_\u0062ody_bytes":2}}`, "duplicate key", "Remove the duplicate key"},
		{"case", `{"Logging":{}}`, "unknown key", "check the spelling"},
		{"wrong_type", "{\n\n\"messages\":{\"max_body_bytes\":\"ten\"}}", "line 3, key \"messages.max_body_bytes\": wrong JSON type", "Use an integer"},
		{"null", `{"logging":{"level":null}}`, "null is not a setting value", "Use a string"},
		{"null_group", `{"logging":null}`, "expected a JSON object", "Use a JSON object"},
		{"version_key", `{"version":1}`, `key "version": unknown key`, "check the spelling"},
		{"schema_key", `{"schema":2}`, `key "schema": unknown key`, "check the spelling"},
		{"trailing", `{} {}`, "extra content follows", "Remove all content after the first JSON object"},
		{"array", `[]`, "expected a JSON object", "Use a JSON object"},
		{"empty", ``, "incomplete JSON object", "Correct the JSON syntax"},
		{"syntax", "{\n\n\"logging\":]}", "line 3", "Correct the JSON syntax"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := fixtureInput(t, nil)
			path := fixtureFile(t, input, test.raw)
			input.Flags["log-level"] = "off"
			_, err := Load(input)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), test.located) || !strings.Contains(err.Error(), test.repair) {
				t.Fatalf("wrong strict diagnostic: %v", err)
			}
		})
	}
}

func TestSettingBoundsAndRepair(t *testing.T) {
	for _, test := range []struct {
		name, raw, repair string
		valid             bool
	}{
		{"body_zero", `{"messages":{"max_body_bytes":0}}`, "integer from 1 through 16384", false},
		{"body_one", `{"messages":{"max_body_bytes":1}}`, "", true},
		{"body_max", `{"messages":{"max_body_bytes":16384}}`, "", true},
		{"body_over", `{"messages":{"max_body_bytes":16385}}`, "integer from 1 through 16384", false},
		{"retention_zero", `{"prune":{"retired_older_than":"0"}}`, "", true},
		{"retention_negative", `{"prune":{"retired_older_than":"-1s"}}`, "nonnegative Go duration", false},
		{"retention_max", `{"prune":{"retired_older_than":"2562047h47m16.854775807s"}}`, "", true},
		{"retention_over", `{"prune":{"retired_older_than":"2562047h47m16.854775808s"}}`, "nonnegative Go duration", false},
		{"log_level", `{"logging":{"level":"do-not-echo-this-value"}}`, "Use debug, info, warn, error, or off", false},
		{"log_path", `{"logging":{"file":"bad\u0000path"}}`, "without control characters (including newlines)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := fixtureInput(t, nil)
			path := fixtureFile(t, input, test.raw)
			_, err := Load(input)
			if test.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), test.repair) || strings.Contains(err.Error(), "do-not-echo-this-value") {
				t.Fatalf("wrong bounds diagnostic: %v", err)
			}
		})
	}
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, "{\n\n\"messages\":{\"max_body_bytes\":0}}")
	if _, err := Load(input); err == nil || !strings.Contains(err.Error(), path+" line 3, key \"messages.max_body_bytes\"") {
		t.Fatalf("file bounds location: %v", err)
	}
}

func TestPrivateFileAndReadBounds(t *testing.T) {
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, `{}`)
	checkPrivateFileModes(t, input, path)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	exact := `{}` + strings.Repeat(" ", maxConfigBytes-len(`{}`))
	if err := os.WriteFile(path, []byte(exact), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(input); err != nil {
		t.Fatalf("exact file limit: %v", err)
	}
	if err := os.WriteFile(path, []byte(exact+" "), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(input); err == nil || !strings.Contains(err.Error(), "at most 65536 bytes") {
		t.Fatalf("oversized file accepted: %v", err)
	}
	if _, err := readBounded(strings.NewReader(exact + " ")); err == nil {
		t.Fatal("growth beyond stat size was accepted")
	}
	if _, err := readBounded(failingReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read failure lost: %v", err)
	}
}

type foreignFileInfo struct {
	os.FileInfo
	owner *syscall.Stat_t
}

func (info foreignFileInfo) Sys() any { return info.owner }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRejectSymlinksDirectoriesAndFIFO(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling_symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			input := fixtureInput(t, nil)
			path := filepath.Join(input.Home, "selected.json")
			input.Flags["config"] = path
			var err error
			switch kind {
			case "symlink", "dangling_symlink":
				target := filepath.Join(input.Home, "target.json")
				if kind == "symlink" {
					if err := os.WriteFile(target, []byte(`{}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				err = os.Symlink(target, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Load(input); err == nil || !strings.Contains(err.Error(), "Choose a regular config file") {
				t.Fatalf("unsafe file accepted: %v", err)
			}
		})
	}
}

type pathChoiceCase struct {
	name, variable, flag, suffix string
	config                       bool
}

func fixtureSelectedPath(paths Paths, configFile bool) Value[string] {
	if configFile {
		return paths.ConfigFile
	}
	return paths.StateDir
}
func checkPathChoicePrecedence(t *testing.T, test pathChoiceCase) {
	t.Helper()
	env := map[string]string{}
	input := fixtureInput(t, env)
	env[test.variable] = filepath.Join(input.Home, "xdg")
	paths, err := ResolvePaths(input)
	selected := fixtureSelectedPath(paths, test.config)
	if err != nil || selected.Value != filepath.Join(env[test.variable], test.suffix) || selected.Source != "env" || selected.Origin != test.variable {
		t.Fatalf("XDG path: %+v, %v", selected, err)
	}
	zalkiEnv := "ZALKI_" + strings.ToUpper(test.flag)
	env[zalkiEnv] = filepath.Join(input.Home, "selected")
	input.Flags[test.flag] = "relative"
	paths, err = ResolvePaths(input)
	selected = fixtureSelectedPath(paths, test.config)
	if err != nil || selected.Value != filepath.Join(input.Cwd, "relative") || selected.Source != "flag" {
		t.Fatalf("flag path: %+v, %v", selected, err)
	}
	delete(input.Flags, test.flag)
	paths, err = ResolvePaths(input)
	selected = fixtureSelectedPath(paths, test.config)
	if err != nil || selected.Value != env[zalkiEnv] || selected.Source != "env" || selected.Origin != zalkiEnv {
		t.Fatalf("Zalki env path: %+v, %v", selected, err)
	}
}

type loggingChoiceCase struct {
	name, variable, flag, env, override string
	read                                func(Settings) Value[string]
}

func checkLoggingSettingPrecedence(t *testing.T, test loggingChoiceCase) {
	t.Helper()
	env := map[string]string{}
	input := fixtureInput(t, env)
	fixtureFile(t, input, `{"logging":{"level":"debug","file":"/file.log"},"prune":{"retired_older_than":"1h"}}`)
	for _, source := range []string{"file", "env", "flag"} {
		if source == "env" {
			env[test.variable] = test.env
		}
		if source == "flag" {
			input.Flags[test.flag] = test.override
		}
		snapshot, err := Load(input)
		value := test.read(snapshot.Settings)
		if err != nil || value.Source != source {
			t.Fatalf("%s source: %+v, %v", source, value, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil || !strings.Contains(string(encoded), `"source":"`+source+`"`) || !strings.Contains(string(encoded), `"origin":`) {
			t.Fatalf("source annotation: %s, %v", encoded, err)
		}
	}
}

func checkPrivateFileModes(t *testing.T, input Inputs, path string) {
	t.Helper()
	for _, mode := range []os.FileMode{0600, 0400, 0644} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := Load(input)
		if mode == 0644 {
			if err == nil || !strings.Contains(err.Error(), "Run chmod 600") {
				t.Fatalf("public mode was accepted: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	owner := *info.Sys().(*syscall.Stat_t)
	owner.Uid++
	if err := checkPrivateFile(foreignFileInfo{FileInfo: info, owner: &owner}, uint32(os.Getuid())); err == nil || !strings.Contains(err.Error(), "owned by the invoking user") {
		t.Fatalf("foreign UID was accepted: %v", err)
	}
}
