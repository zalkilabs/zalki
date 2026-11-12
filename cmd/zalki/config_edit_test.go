package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConfigEditKeysAndArgs(t *testing.T) {
	home := configTestHome(t)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"set"}, "use: zalki config set KEY VALUE"},
		{[]string{"set", "logging.level"}, "use: zalki config set KEY VALUE"},
		{[]string{"unset"}, "use: zalki config unset KEY"},
		{[]string{"unset", "logging.level", "debug"}, "use: zalki config unset KEY"},
		{[]string{"set", "logging", "debug"}, `unknown config key "logging". Valid keys: codex.allow_queue_fallback, logging.file, logging.level, messages.max_body_bytes, prune.retired_older_than`},
		{[]string{"set", "crossSessionInbound", "true"}, "crossSessionInbound is owner-only. Zalki will not change it."},
	} {
		out, diagnostic, err := execute(t, home, "", append([]string{"config"}, test.args...)...)
		if err == nil || err.Error() != test.want || out != "" || diagnostic != "" {
			t.Fatal("bad command not refused exactly", test.args, out, diagnostic, err)
		}
	}
}

func TestConfigEditReportsEffectiveSource(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, "selected.json")
	for _, test := range []struct {
		key, value string
		want       any
	}{
		{"messages.max_body_bytes", "123", float64(123)}, {"logging.level", "WARN", "warn"},
		{"logging.file", "relative file.log", filepath.Join(home, "relative file.log")},
		{"prune.retired_older_than", "24h", "24h0m0s"}, {"codex.allow_queue_fallback", "true", true},
	} {
		out, diagnostic, err := execute(t, home, "", "--config", path, "config", "set", test.key, test.value)
		checkEditFileResult(t, path, test.key, test.want, out, diagnostic, err)
	}
	t.Setenv("ZALKI_LOG_LEVEL", "error")
	out, _, err := execute(t, home, "", "--config", path, "config", "set", "logging.level", "debug")
	want := "{\"key\":\"logging.level\",\"value\":\"error\",\"source\":\"env\",\"origin\":\"ZALKI_LOG_LEVEL\"}\n"
	if err != nil || out != want {
		t.Fatal("override result", out, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"debug"`) {
		t.Fatal("override prevented saving requested file value")
	}
	out, _, err = execute(t, home, "", "--config", path, "--log-level", "off", "config", "unset", "logging.level")
	if err != nil || out != "{\"key\":\"logging.level\",\"value\":\"off\",\"source\":\"flag\",\"origin\":\"--log-level\"}\n" {
		t.Fatal("flag override result", out, err)
	}
}

func checkEditFileResult(t *testing.T, path, key string, want any, out, diagnostic string, err error) {
	t.Helper()
	var got configEditResult
	if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatal("invalid result", out, diagnostic, err)
	}
	if !reflect.DeepEqual(got.Value, want) || got.Source != "file" || got.Origin != path+"#"+key || got.Key != key {
		t.Fatal("wrong effective result", got, want)
	}
	if key == "codex.allow_queue_fallback" && got.Exposure != codexQueueFallbackExposure {
		t.Fatal("missing exposure", got)
	}
}

func TestConfigEditHasOnlySelectedFileEffects(t *testing.T) {
	home := configTestHome(t)
	path, state, log := filepath.Join(home, "selected.json"), filepath.Join(home, "state"), filepath.Join(home, "diagnostic.jsonl")
	t.Setenv("ZALKI_LOG_LEVEL", "debug")
	t.Setenv("ZALKI_LOG_FILE", log)
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, filepath.Join(home, "client-trap"))
	}
	out, diagnostic, err := execute(t, home, "", "--config", path, "--state", state, "config", "set", "logging.level", "warn")
	if err != nil || diagnostic != "" || !json.Valid([]byte(out)) {
		t.Fatal(out, diagnostic, err)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 1 || entries[0].Name() != "selected.json" {
		t.Fatal("config edit initialized unrelated files", entries)
	}
}

func TestConfigEditPinnedClientPath(t *testing.T) {
	home := configTestHome(t)
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, "")
	}
	path, other, state := filepath.Join(home, "p.json"), filepath.Join(home, "q.json"), filepath.Join(home, "state")
	writeConfigFixture(t, path, `{}`)
	if _, _, err := execute(t, home, "", "--config", path, "--state", state, "install", "--apply"); err != nil {
		t.Fatal(err)
	}
	before := bootstrapSnapshot(t, home)
	if _, _, err := execute(t, home, "", "--config", path, "config", "set", "messages.max_body_bytes", "123"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, home, "", "--config", other, "config", "set", "messages.max_body_bytes", "321"); err != nil {
		t.Fatal(err)
	}
	after := bootstrapSnapshot(t, home)
	delete(before, path)
	delete(after, path)
	delete(after, other)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("value edit or other path retargeted managed commands")
	}
	out, _, err := execute(t, home, "", "--config", path, "config", "show")
	var show configShowOutput
	if err != nil || json.Unmarshal([]byte(out), &show) != nil || show.Settings.Messages.MaxBodyBytes.Value != 123 {
		t.Fatal("fresh invocation did not load changed pinned file", out, err)
	}
}

func runEditCLI(t *testing.T, binary string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary, args...)
	var out, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	err := cmd.Run()
	return out.String(), diagnostic.String(), err
}

func TestConfigSetUnsetCLI(t *testing.T) {
	binary := checkRealBinary(t)
	home := configTestHome(t)
	t.Setenv("HOME", home)
	path := filepath.Join(home, "private", "config.json")
	checkConfigCLISet(t, binary, path)
	checkConfigCLIUnset(t, binary, path)
}

func checkConfigCLISet(t *testing.T, binary, path string) {
	t.Helper()
	out, diagnostic, err := runEditCLI(t, binary, "--config", path, "config", "set", "codex.allow_queue_fallback", "true")
	var result configEditResult
	if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &result) != nil || result.Value != true || result.Source != "file" || result.Origin != path+"#codex.allow_queue_fallback" || result.Exposure != codexQueueFallbackExposure {
		t.Fatal("real setter", out, diagnostic, err)
	}
	info, _ := os.Stat(path)
	dir, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700 {
		t.Fatal("real private modes", info, dir)
	}
}

func checkConfigCLIUnset(t *testing.T, binary, path string) {
	t.Helper()
	var result configEditResult
	out, diagnostic, err := runEditCLI(t, binary, "--config", path, "config", "unset", "codex.allow_queue_fallback")
	if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &result) != nil || result.Value != false || result.Source != "default" {
		t.Fatal("real unset", out, diagnostic, err)
	}
	before, _ := os.ReadFile(path)
	out, diagnostic, err = runEditCLI(t, binary, "--config", path, "config", "set", "CrossSessionInbound", "true")
	if err == nil || out != "" || diagnostic != "zalki: crossSessionInbound is owner-only. Zalki will not change it.\n" {
		t.Fatal("real owner refusal", out, diagnostic, err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != "{}\n" || !bytes.Equal(before, after) {
		t.Fatal("real unset/owner bytes", string(after))
	}
}

func TestConfigEditTypedDiagnostic(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, "selected.json")
	out, diagnostic, err := execute(t, home, "", "--config", path, "config", "set", "codex.allow_queue_fallback", "1")
	want := path + " line 3, key \"codex.allow_queue_fallback\": wrong JSON type. Use true or false for this key, or remove the optional key to inherit its default"
	if err == nil || err.Error() != want || out != "" || diagnostic != "" {
		t.Fatal("typed value diagnostic", out, diagnostic, err)
	}
	out, diagnostic, err = execute(t, home, "", "--config", path, "config", "set", "codex.allow_queue_fallback", "TRUE")
	want = "invalid value for \"codex.allow_queue_fallback\". Use true or false for this key, or remove the optional key to inherit its default"
	if err == nil || err.Error() != want || out != "" || diagnostic != "" {
		t.Fatal("JSON token diagnostic", out, diagnostic, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("bad value created a file", err)
	}
}
