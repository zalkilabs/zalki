package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestLoggingFlagsPreserveJSONStdout(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	input := `{"session_id":"00000000-0000-4000-8000-000000000031","cwd":"/fixture","hook_event_name":"SessionStart"}`
	out, stderr, err := execute(t, home, input, "--state", state, "--log-level", "debug", "--log-file", "-", "hook", "--agent", "codex")
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatal("logging contaminated hook protocol stdout", err)
	}
	var result struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output.Context, "private capability") {
		t.Fatal("startup credentials disappeared")
	}
	if strings.Contains(stderr, "private capability") || strings.Contains(stderr, "ZALKI_CAPABILITY") || strings.Contains(stderr, "/fixture") {
		t.Fatal("startup payload leaked to stderr logs")
	}
	scanner := bufio.NewScanner(strings.NewReader(stderr))
	count := 0
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil || row["app"] != "zalki" || row["version"] != bus.BuildVersion() || row["pid"] == nil || row["component"] != "hook" {
			t.Fatal("invalid CLI log context", err)
		}
		count++
	}
	if count < 3 {
		t.Fatal("missing command or hook events")
	}
	if _, err := os.Stat(filepath.Join(state, "logs")); !os.IsNotExist(err) {
		t.Fatal("stderr-only mode created logs", err)
	}
}

func TestLoggingEnvironmentFlagsAndDisabledMode(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "diagnostics", "events.jsonl")
	t.Setenv("ZALKI_LOG_LEVEL", "debug")
	t.Setenv("ZALKI_LOG_FILE", path)
	if _, stderr, err := execute(t, home, "", "version"); err != nil || stderr != "" {
		t.Fatal(err, stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "command.completed") {
		t.Fatal("environment configuration was ignored", err)
	}
	before := string(data)
	if _, stderr, err := execute(t, home, "", "version", "--log-level", "off"); err != nil || stderr != "" {
		t.Fatal(err, stderr)
	}
	data, _ = os.ReadFile(path)
	if string(data) != before {
		t.Fatal("flag did not override environment or off mode wrote logs")
	}
	if _, _, err := execute(t, home, "", "version", "--log-level", "invalid"); err == nil {
		t.Fatal("invalid level accepted")
	}
}

func TestDefaultLoggingCapturesDeliveryAndKeepsPreviewsReadOnly(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	input := `{"session_id":"00000000-0000-4000-8000-000000000032","cwd":"/fixture","hook_event_name":"SessionStart"}`
	if _, stderr, err := execute(t, home, input, "hook", "--agent", "codex", "--state", state); err != nil || stderr != "" {
		t.Fatal(err, stderr)
	}
	path := filepath.Join(state, "logs", "zalki.jsonl")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "session.registered") {
		t.Fatal("default log missing", err)
	}
	before := string(data)
	if _, stderr, err := execute(t, home, "", "uninstall", "--state", state); err != nil || stderr != "" {
		t.Fatal(err, stderr)
	}
	data, _ = os.ReadFile(path)
	if string(data) != before {
		t.Fatal("default-level preview appended logs")
	}
}
