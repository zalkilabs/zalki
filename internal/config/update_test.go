package config

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func editValue(t *testing.T, input Inputs, key, value string) Snapshot {
	t.Helper()
	result, err := Update(input, key, &value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestConfigEditKeys(t *testing.T) {
	want := []string{"codex.allow_queue_fallback", "logging.file", "logging.level", "messages.max_body_bytes", "prune.retired_older_than"}
	if !reflect.DeepEqual(editableKeys(), want) {
		t.Fatal("schema leaves differ", editableKeys())
	}
	for _, key := range []string{"logging", "config", "state", "schema", "logging.lvl", "messages.max_body_bytes.extra", "bad\nkey"} {
		input := fixtureInput(t, nil)
		_, err := Update(input, key, nil)
		expected := "unknown config key " + string(mustJSON(t, key)) + ". Valid keys: " + strings.Join(want, ", ")
		if err == nil || err.Error() != expected {
			t.Fatalf("key %q: %v", key, err)
		}
		entries, _ := os.ReadDir(input.Home)
		if len(entries) != 0 {
			t.Fatal("invalid key wrote files")
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestConfigEditOwnerOnlyKey(t *testing.T) {
	for _, key := range []string{"crossSessionInbound", "CrossSessionInbound", "logging.crossSessionInbound", "a.CROSSSESSIONINBOUND.b"} {
		for _, value := range []*string{nil, new(string)} {
			input := Inputs{Home: "invalid home", Flags: map[string]string{"config": "bad\npath"}}
			_, err := Update(input, key, value)
			if !errors.Is(err, errOwnerOnly) {
				t.Fatalf("protected key reached path resolution: %q %v", key, err)
			}
		}
	}
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, `{"crossSessionInbound":true}`)
	_, err := Update(input, "logging.level", nil)
	raw, _ := os.ReadFile(path)
	if !errors.Is(err, errOwnerOnly) || string(raw) != `{"crossSessionInbound":true}` {
		t.Fatal("owner key changed or deletion advised", err, string(raw))
	}
}

func TestConfigEditValueValidation(t *testing.T) {
	cases := []struct{ key, value string }{
		{"codex.allow_queue_fallback", "TRUE"}, {"codex.allow_queue_fallback", "1"}, {"codex.allow_queue_fallback", "null"},
		{"codex.allow_queue_fallback", `"true"`}, {"codex.allow_queue_fallback", "true false"}, {"codex.allow_queue_fallback", "{}"},
		{"messages.max_body_bytes", "0"}, {"messages.max_body_bytes", "16385"}, {"messages.max_body_bytes", "1.5"},
		{"messages.max_body_bytes", `"12"`}, {"messages.max_body_bytes", "null"}, {"logging.level", "bogus"},
		{"logging.file", "bad\npath"}, {"prune.retired_older_than", "-1h"}, {"prune.retired_older_than", "999999999h"},
	}
	for _, test := range cases {
		t.Run(test.key+"/"+test.value, func(t *testing.T) {
			input := fixtureInput(t, nil)
			path := fixtureFile(t, input, "{}\n")
			_, err := Update(input, test.key, &test.value)
			raw, _ := os.ReadFile(path)
			if err == nil || string(raw) != "{}\n" {
				t.Fatal("invalid value published", err, string(raw))
			}
		})
	}
}

func TestConfigEditRefusesExistingInvalidFile(t *testing.T) {
	for _, raw := range []string{"", "{", `null`, `{"logging":null}`, `{"logging":{"level":null}}`, `{"bad":1}`, `{"logging":{"level":"off","level":"warn"}}`, `{} {}`, `{"logging":{"level":"invalid"}}`} {
		t.Run(raw, func(t *testing.T) {
			input := fixtureInput(t, map[string]string{"ZALKI_LOG_LEVEL": "off"})
			path := fixtureFile(t, input, raw)
			value := "debug"
			_, err := Update(input, "logging.level", &value)
			after, _ := os.ReadFile(path)
			if err == nil || string(after) != raw {
				t.Fatal("bad existing file rewritten", err, string(after))
			}
		})
	}
}

func TestConfigSetUnsetPreservesOtherSettings(t *testing.T) {
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, `{"logging":{"file":"keep.log","level":"warn"},"messages":{"max_body_bytes":123}}`)
	result := editValue(t, input, "codex.allow_queue_fallback", "true")
	if !result.Settings.CodexAllowQueueFallback.Value || result.Settings.MaxBodyBytes.Value != 123 || result.Settings.LogLevel.Value != "warn" || result.Settings.LogFile.Value != filepath.Join(filepath.Dir(path), "keep.log") {
		t.Fatal("siblings lost", result)
	}
	result, err := Update(input, "codex.allow_queue_fallback", nil)
	if err != nil || result.Settings.CodexAllowQueueFallback.Value || result.Settings.CodexAllowQueueFallback.Source != "default" {
		t.Fatal("unset did not inherit", result, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "codex") || !strings.Contains(string(raw), "keep.log") {
		t.Fatal("unset wrote default or lost siblings", string(raw))
	}
	before := string(raw)
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(input, "codex.allow_queue_fallback", nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(after) != before || info.Mode().Perm() != 0400 {
		t.Fatal("absent unset changed bytes/mode")
	}
}

func TestConfigEditMissingUnsetAndLastLeaf(t *testing.T) {
	input := fixtureInput(t, nil)
	result, err := Update(input, "logging.level", nil)
	entries, _ := os.ReadDir(input.Home)
	if err != nil || result.FileExists || len(entries) != 0 {
		t.Fatal("missing unset created file", err, entries)
	}
	path := fixtureFile(t, input, `{"logging":{"level":"warn"}}`)
	if _, err := Update(input, "logging.level", nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{}\n" {
		t.Fatal("last leaf should leave empty object", string(raw))
	}
}

func TestConfigEditPrivateModes(t *testing.T) {
	input := fixtureInput(t, nil)
	result := editValue(t, input, "logging.level", "debug")
	for path, mode := range map[string]os.FileMode{result.Paths.ConfigFile.Value: 0600, result.Paths.ConfigDir: 0700, filepath.Dir(result.Paths.ConfigDir): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("wrong private mode", path, info, err)
		}
	}
	if err := os.Chmod(result.Paths.ConfigFile.Value, 0400); err != nil {
		t.Fatal(err)
	}
	editValue(t, input, "logging.level", "WARN")
	info, _ := os.Stat(result.Paths.ConfigFile.Value)
	if info.Mode().Perm() != 0600 {
		t.Fatal("read-only source did not publish 0600", info.Mode())
	}
}

func TestConfigEditPermissiveUmask(t *testing.T) {
	if os.Getenv("ZALKI_EDIT_UMASK_CHILD") == "1" {
		syscall.Umask(0)
		TestConfigEditPrivateModes(t)
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConfigEditPermissiveUmask$")
	cmd.Env = append(os.Environ(), "ZALKI_EDIT_UMASK_CHILD=1")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(raw))
	}
}
