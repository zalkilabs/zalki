package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// The doctor reads the files the installer wrote. These tests write the declared
// patches by hand into a temporary home: the real installer is used in the tests
// of package adapters.

const diagnoseBinary = "/opt/zalki tools/zalki"

type diagnoseFixture struct {
	home  string
	input agent.DiagnosticInput
}

func newDiagnoseFixture(t *testing.T) diagnoseFixture {
	t.Helper()
	home := t.TempDir()
	in := agent.DiagnosticInput{
		Home: home, StateDir: filepath.Join(home, "state"), ConfigFile: filepath.Join(home, "config.json"), Binary: diagnoseBinary,
		Found: true, Version: "1.5.18",
	}
	f := diagnoseFixture{home: home, input: in}
	files := New().Configuration(diagnoseBinary, []string{"--config", in.ConfigFile, "--state", in.StateDir})
	f.write(t, hooksPath, files[0].JSON)
	f.write(t, mcpPath, files[1].JSON)
	f.input.RecordedTargets = map[string]string{hooksPath: f.path(hooksPath), mcpPath: f.path(mcpPath)}
	return f
}

func (f diagnoseFixture) path(declared string) string {
	return filepath.Join(f.home, filepath.FromSlash(declared))
}

func (f diagnoseFixture) write(t *testing.T, declared string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	f.writeRaw(t, declared, string(raw))
}

func (f diagnoseFixture) writeRaw(t *testing.T, declared, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(declared)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path(declared), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// edit changes one of the files the way a user could.
func (f diagnoseFixture) edit(t *testing.T, declared string, change func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(f.path(declared))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	change(top)
	f.write(t, declared, top)
}

func (f diagnoseFixture) diagnose(t *testing.T) diagnostic {
	t.Helper()
	key, section := New().Diagnose(f.input)
	got, ok := section.(diagnostic)
	if key != "antigravity" || !ok {
		t.Fatalf("section %q has type %T", key, section)
	}
	if _, err := json.Marshal(section); err != nil {
		t.Fatalf("the section cannot be encoded: %v", err)
	}
	return got
}

func (d diagnostic) codes() string {
	codes := make([]string, 0, len(d.Problems))
	for _, p := range d.Problems {
		codes = append(codes, p.Code)
	}
	return strings.Join(codes, ",")
}

func requireDiagnosis(t *testing.T, got diagnostic, status, codes string) {
	t.Helper()
	if got.Status != status || got.codes() != codes {
		t.Fatalf("status %q with problems [%s], want %q with [%s]: %+v", got.Status, got.codes(), status, codes, got)
	}
}

func TestDiagnoseAcceptsWhatTheInstallerWrote(t *testing.T) {
	got := newDiagnoseFixture(t).diagnose(t)
	requireDiagnosis(t, got, "ok", "")
	if got.Message != "Antigravity: the Zalki hooks and MCP entry are installed." {
		t.Fatalf("message = %q", got.Message)
	}
}

func TestDiagnoseIgnoresWhatOtherToolsAndTheUserKeepInTheFiles(t *testing.T) {
	f := newDiagnoseFixture(t)
	f.edit(t, hooksPath, func(top map[string]any) {
		top["other-tool"] = map[string]any{"enabled": false, "Stop": []any{map[string]any{"type": "command", "command": "echo hi"}}}
		top["zalki"].(map[string]any)["note"] = "mine"
	})
	f.edit(t, mcpPath, func(top map[string]any) {
		servers := top["mcpServers"].(map[string]any)
		servers["other"] = map[string]any{"command": "x"}
		servers["zalki"].(map[string]any)["env"] = map[string]any{"A": "b"}
	})
	requireDiagnosis(t, f.diagnose(t), "ok", "")
}

func TestDiagnoseReportsAnEntryTheUserDisabled(t *testing.T) {
	f := newDiagnoseFixture(t)
	f.edit(t, hooksPath, func(top map[string]any) { top["zalki"].(map[string]any)["enabled"] = false })
	got := f.diagnose(t)
	requireDiagnosis(t, got, "attention", "entry_disabled")
	want := `Antigravity: the "zalki" entry has "enabled": false, so Antigravity does not run the Zalki hooks. Remove that key to turn them on.`
	if got.Problems[0].Message != want {
		t.Fatalf("message = %q, want %q", got.Problems[0].Message, want)
	}
	f.edit(t, hooksPath, func(top map[string]any) { top["zalki"].(map[string]any)["enabled"] = true })
	requireDiagnosis(t, f.diagnose(t), "ok", "")
}

func TestDiagnoseReportsTheOtherProblemsOfTheHooksFile(t *testing.T) {
	for name, test := range map[string]struct {
		change func(map[string]any)
		codes  string
	}{
		"no zalki entry":    {func(top map[string]any) { delete(top, "zalki") }, "entry_missing"},
		"entry is a string": {func(top map[string]any) { top["zalki"] = "x" }, "entry_missing"},
		"an event is gone":  {func(top map[string]any) { delete(top["zalki"].(map[string]any), "PreInvocation") }, "event_missing"},
		"a command differs": {func(top map[string]any) {
			top["zalki"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["command"] = "mine"
		}, "command_differs"},
		"an added handler": {func(top map[string]any) {
			e := top["zalki"].(map[string]any)
			e["Stop"] = append(e["Stop"].([]any), map[string]any{"command": "x"})
		}, "command_differs"},
		"disabled and moved": {func(top map[string]any) {
			top["zalki"].(map[string]any)["enabled"] = false
			delete(top["zalki"].(map[string]any), "Stop")
		}, "event_missing,entry_disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDiagnoseFixture(t)
			f.edit(t, hooksPath, test.change)
			requireDiagnosis(t, f.diagnose(t), "attention", test.codes)
		})
	}
}

func TestDiagnoseReportsTheSameHookUnderAnotherName(t *testing.T) {
	for name, command := range map[string]string{
		"quoted":      "'/opt/zalki tools/zalki' 'hook' '--agent' 'antigravity'",
		"equals sign": "zalki hook --agent=antigravity",
		"env prefix":  "ZALKI_X=1 /usr/local/bin/zalki hook --agent antigravity > /dev/null",
	} {
		t.Run(name, func(t *testing.T) {
			f := newDiagnoseFixture(t)
			f.edit(t, hooksPath, func(top map[string]any) {
				top["copy-of-zalki"] = map[string]any{"Stop": []any{map[string]any{"type": "command", "command": command}}}
				top["innocent"] = map[string]any{"Stop": []any{map[string]any{"type": "command", "command": "echo hook --agent claude"}}}
			})
			got := f.diagnose(t)
			requireDiagnosis(t, got, "attention", "duplicate_hook")
			want := "Antigravity: the entries copy-of-zalki also run the Zalki hook, so it runs twice. Delete the extra entry."
			if got.Problems[0].Message != want {
				t.Fatalf("message = %q, want %q", got.Problems[0].Message, want)
			}
		})
	}
}

func TestDiagnoseReportsTheMCPFileProblems(t *testing.T) {
	for name, test := range map[string]struct {
		change func(map[string]any)
		codes  string
	}{
		"no zalki server": {func(top map[string]any) { delete(top["mcpServers"].(map[string]any), "zalki") }, "mcp_entry_missing"},
		"no servers":      {func(top map[string]any) { delete(top, "mcpServers") }, "mcp_entry_missing"},
		"a command differs": {func(top map[string]any) {
			top["mcpServers"].(map[string]any)["zalki"].(map[string]any)["command"] = "mine"
		}, "mcp_entry_differs"},
		"an arg differs": {func(top map[string]any) {
			top["mcpServers"].(map[string]any)["zalki"].(map[string]any)["args"] = []any{"mcp"}
		}, "mcp_entry_differs"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDiagnoseFixture(t)
			f.edit(t, mcpPath, test.change)
			requireDiagnosis(t, f.diagnose(t), "attention", test.codes)
		})
	}
}

func TestDiagnoseReportsFilesItCannotUse(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f := newDiagnoseFixture(t)
		if err := os.Remove(f.path(hooksPath)); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(f.path(mcpPath)); err != nil {
			t.Fatal(err)
		}
		got := f.diagnose(t)
		requireDiagnosis(t, got, "attention", "hooks_file_missing_file,mcp_file_missing_file")
		if want := "Antigravity: the hooks file is missing file. Run zalki upgrade, or fix the file."; got.Problems[0].Message != want {
			t.Fatalf("message = %q", got.Problems[0].Message)
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		f := newDiagnoseFixture(t)
		f.writeRaw(t, hooksPath, `{"zalki":`)
		f.writeRaw(t, mcpPath, `[1]`)
		requireDiagnosis(t, f.diagnose(t), "attention", "hooks_file_invalid_data,mcp_file_invalid_data")
	})
	t.Run("a link", func(t *testing.T) {
		f := newDiagnoseFixture(t)
		target := filepath.Join(f.home, "elsewhere.json")
		if err := os.Rename(f.path(hooksPath), target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, f.path(hooksPath)); err != nil {
			t.Skipf("links are not available: %v", err)
		}
		requireDiagnosis(t, f.diagnose(t), "attention", "hooks_file_unsafe_file")
	})
	t.Run("a folder", func(t *testing.T) {
		f := newDiagnoseFixture(t)
		if err := os.Remove(f.path(hooksPath)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(f.path(hooksPath), 0o700); err != nil {
			t.Fatal(err)
		}
		requireDiagnosis(t, f.diagnose(t), "attention", "hooks_file_unsafe_file")
	})
	t.Run("too big", func(t *testing.T) {
		f := newDiagnoseFixture(t)
		f.writeRaw(t, hooksPath, `{"a":"`+strings.Repeat("x", maxClientFile)+`"}`)
		requireDiagnosis(t, f.diagnose(t), "attention", "hooks_file_unsafe_file")
	})
}

func TestDiagnoseUsesTheFileTheRecordNames(t *testing.T) {
	f := newDiagnoseFixture(t)
	custom := filepath.Join(f.home, "elsewhere", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(custom), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.path(hooksPath), custom); err != nil {
		t.Fatal(err)
	}
	requireDiagnosis(t, f.diagnose(t), "attention", "hooks_file_missing_file")
	f.input.RecordedTargets[hooksPath] = custom
	requireDiagnosis(t, f.diagnose(t), "ok", "")
}

func TestDiagnoseKnowsWhenZalkiIsNotInstalledForAntigravity(t *testing.T) {
	f := newDiagnoseFixture(t)
	f.input.RecordReason, f.input.RecordedTargets = "missing_file", nil
	requireDiagnosis(t, f.diagnose(t), "ok", "") // no record, but the entries are there: judge the files
	f.edit(t, hooksPath, func(top map[string]any) { delete(top, "zalki") })
	got := f.diagnose(t)
	requireDiagnosis(t, got, "not_installed", "")
	if got.Message != "Antigravity: Zalki is not installed for Antigravity. Run zalki install." {
		t.Fatalf("message = %q", got.Message)
	}
	if err := os.Remove(f.path(hooksPath)); err != nil {
		t.Fatal(err)
	}
	requireDiagnosis(t, f.diagnose(t), "not_installed", "")
}

func TestDiagnoseCannotVerifyWithoutTheFactsItNeeds(t *testing.T) {
	for name, change := range map[string]func(*agent.DiagnosticInput){
		"a relative binary":   func(in *agent.DiagnosticInput) { in.Binary = "zalki" },
		"no binary":           func(in *agent.DiagnosticInput) { in.Binary = "" },
		"a relative home":     func(in *agent.DiagnosticInput) { in.Home = "home" },
		"an invalid record":   func(in *agent.DiagnosticInput) { in.RecordReason = "invalid_data" },
		"an ambiguous record": func(in *agent.DiagnosticInput) { in.RecordReason = "ambiguous_ownership" },
		"an unsafe record":    func(in *agent.DiagnosticInput) { in.RecordReason = "unsafe_file" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDiagnoseFixture(t)
			change(&f.input)
			got := f.diagnose(t)
			requireDiagnosis(t, got, "cannot_verify", "")
			if !strings.HasPrefix(got.Message, "Antigravity: cannot verify the Zalki entries") {
				t.Fatalf("message = %q", got.Message)
			}
		})
	}
}
