package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestConfigRuntimeLogging(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, ".config", "zalki", "config.json")
	log := filepath.Join(filepath.Dir(path), "configured.jsonl")
	writeConfigFixture(t, path, `{"logging":{"level":"debug","file":"configured.jsonl"}}`)
	if _, diagnostic, err := execute(t, home, "", "version"); err != nil || diagnostic != "" {
		t.Fatal("file logging failed", err, diagnostic)
	}
	before, err := os.ReadFile(log)
	if err != nil || !bytes.Contains(before, []byte(`"msg":"command.completed"`)) {
		t.Fatal("file level/path did not configure runtime logging", err)
	}
	if _, diagnostic, err := execute(t, home, "", "version", "--log-level", "off"); err != nil || diagnostic != "" {
		t.Fatal("logging flag override failed", err, diagnostic)
	}
	after, err := os.ReadFile(log)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("off flag did not suppress file logging", err)
	}
	_, diagnostic, err := execute(t, home, "", "version", "--log-file", "-")
	if err != nil || !strings.Contains(diagnostic, `"msg":"command.completed"`) {
		t.Fatal("stderr flag did not override the configured file", err, diagnostic)
	}
	writeConfigFixture(t, path, `{"logging":{"level":"off","file":"configured.jsonl"}}`)
	if _, diagnostic, err := execute(t, home, "", "version"); err != nil || diagnostic != "" {
		t.Fatal("file off level failed", err, diagnostic)
	}
	after, err = os.ReadFile(log)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("file off level still wrote events", err)
	}
}

func TestConfigLogFileCannotOverwriteSelectedConfig(t *testing.T) {
	for _, alias := range []string{"direct", "hardlink", "symlink", "missing"} {
		t.Run(alias, func(t *testing.T) {
			home := configTestHome(t)
			configFile := filepath.Join(home, "config.json")
			if alias != "missing" {
				writeConfigFixture(t, configFile, `{}`)
			}
			logFile := configFile
			if alias == "hardlink" || alias == "symlink" {
				logFile = filepath.Join(home, "log-alias")
				link := os.Link
				if alias == "symlink" {
					link = os.Symlink
				}
				if err := link(configFile, logFile); err != nil {
					t.Fatal(err)
				}
			}
			before := cliSnapshot(t, home)
			out, diagnostic, err := execute(t, home, "", "--config", configFile, "--log-level=debug", "--log-file", logFile, "version")
			if err == nil || out != "" || diagnostic != "" || !strings.Contains(err.Error(), "--log-file points at the selected config file") || !reflect.DeepEqual(before, cliSnapshot(t, home)) {
				t.Fatal("diagnostic sink could overwrite the selected settings file", err)
			}
		})
	}
}

func TestConfigRuntimeLazyLogging(t *testing.T) {
	for _, raw := range []string{`{"logging":{"level":"debug"}}`, `{"logging":{"level":"debug","file":""}}`} {
		t.Run(raw, func(t *testing.T) {
			home := configTestHome(t)
			path := filepath.Join(home, ".config", "zalki", "config.json")
			writeConfigFixture(t, path, raw)
			if _, diagnostic, err := execute(t, home, "", "version"); err != nil || diagnostic != "" {
				t.Fatal(err, diagnostic)
			}
			if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
				t.Fatal("derived default sink created state before the journal", err)
			}
			explicit := filepath.Join(home, ".local", "state", "zalki", "logs", "zalki.jsonl")
			if _, _, err := execute(t, home, "", "version", "--log-file", explicit); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(explicit); err != nil {
				t.Fatal("explicit sink was incorrectly treated as lazy", err)
			}
		})
	}
}

func TestConfigRuntimeSilentPluginProxy(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, ".config", "zalki", "config.json")
	writeConfigFixture(t, path, `{"logging":{"level":"debug","file":"unused.jsonl"}}`)
	out, diagnostic, err := execute(t, home, "", "plugins", "read", "fixture", "--session", "codex:00000000-0000-4000-8000-000000000001", "--message", "00000000-0000-4000-8000-000000000002")
	if err == nil || out != "" || diagnostic != "" {
		t.Fatalf("proxy diagnostics changed: %v %q %q", err, out, diagnostic)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "unused.jsonl")); !os.IsNotExist(err) {
		t.Fatal("service-only resolution opened the file logger", err)
	}
}

func TestConfigRuntimePruneRetention(t *testing.T) {
	home := configTestHome(t)
	state := filepath.Join(home, "registry")
	store, err := bus.NewStore(state, adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	from, key, err := store.Register("codex", model.NewUUID(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := store.Register("codex", model.NewUUID(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := store.ReserveMessage(bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: model.NewUUID(), Body: "retained history"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(from.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "zalki", "config.json")
	writeConfigFixture(t, path, `{"prune":{"retired_older_than":"0"},"messages":{"max_body_bytes":4},"logging":{"level":"off"}}`)
	run := func(extra ...string) bus.PruneReport {
		t.Helper()
		args := append([]string{"--state", state, "prune"}, extra...)
		out, diagnostic, err := execute(t, home, "", args...)
		var report bus.PruneReport
		if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &report) != nil {
			t.Fatalf("configured prune: %v %q %s", err, diagnostic, out)
		}
		return report
	}
	if report := run("--dry-run"); len(report.DeletedSessions) != 1 {
		t.Fatal("file retention was ignored", report.DeletedSessions)
	}
	if report := run("--dry-run", "--retired-older-than", "24h"); len(report.DeletedSessions) != 0 {
		t.Fatal("prune-local flag did not override the file", report.DeletedSessions)
	}
	writeConfigFixture(t, path, `{"prune":{"retired_older_than":"168h"},"messages":{"max_body_bytes":4},"logging":{"level":"off"}}`)
	if report := run("--dry-run", "--retired-older-than", "0"); len(report.DeletedSessions) != 1 {
		t.Fatal("explicit zero flag was treated as absent", report.DeletedSessions)
	}
	writeConfigFixture(t, path, `{"prune":{"retired_older_than":"0"},"messages":{"max_body_bytes":4},"logging":{"level":"off"}}`)
	if report := run(); len(report.DeletedSessions) != 1 || report.DryRun {
		t.Fatal("file retention did not apply", report.DeletedSessions)
	}
	if _, err := os.Stat(filepath.Join(state, "messages", msg.ID+".json")); err != nil {
		t.Fatal("prune deleted message history", err)
	}
}

func TestConfigRuntimeBodyCap(t *testing.T) {
	home := configTestHome(t)
	state := filepath.Join(home, "registry")
	store, err := bus.NewStore(state, adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	from, key, err := store.Register("codex", model.NewUUID(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := store.Register("codex", model.NewUUID(), home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(to.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, filepath.Join(home, ".config", "zalki", "config.json"), `{"messages":{"max_body_bytes":4},"logging":{"level":"off"}}`)
	t.Setenv("ZALKI_CAPABILITY", key)
	input, err := json.Marshal(map[string]string{"actor": from.ID, "to": to.ID, "message_id": model.NewUUID(), "body": "12345678"})
	if err != nil {
		t.Fatal(err)
	}
	out, diagnostic, err := execute(t, home, string(input), "--state", state, "send")
	if model.ErrorCodeOf(err) != model.CodeInvalidInput || err.Error() != "invalid_input: body is 8 UTF-8 bytes; configured maximum is 4 bytes" || out != "" || diagnostic != "" {
		t.Fatalf("runtime cap differs from configured cap: %v %q %q", err, out, diagnostic)
	}
	for _, dir := range []string{"messages", "flood", "open-messages"} {
		entries, err := os.ReadDir(filepath.Join(state, dir))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatal("oversize CLI send changed admission state", dir)
		}
	}
}
