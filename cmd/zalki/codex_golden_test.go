package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// These tests pin, byte for byte, what `zalki doctor` reports in its codex_hooks
// section and what install, uninstall and upgrade print. They were recorded
// from the code before the Codex trust engine moved out of this package and
// must not change when it moves. Run with -update-golden only to record anew.
var updateGolden = flag.Bool("update-golden", false, "rewrite the golden files in testdata/codex_golden")

// checkGolden compares got with the golden file; home is replaced by $HOME so that the
// temporary directory does not matter.
func checkGolden(t *testing.T, name, home, got string) {
	t.Helper()
	got = strings.ReplaceAll(got, home, "$HOME")
	path := filepath.Join("testdata", "codex_golden", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from the golden file:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

type codexGoldenCase struct {
	name     string
	version  string // the Codex version in the doctor report; "none" leaves Codex out of the report
	setup    func(t *testing.T, home, state, root string, hooks []byte)
	sessions bool
}

func codexGoldenCases() []codexGoldenCase {
	trust := func(t *testing.T, home, state, root string, hooks []byte) {
		opts, _, _ := newCodexDiagnosticFixture(t, home, state, root)
		expected, err := expectedCodexHookHashes(opts)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "hooks.json")
		var states strings.Builder
		for _, row := range diagnoseCodexHooks(opts, bus.DiagnosticReport{SessionScanComplete: true, Agents: []bus.AgentDiagnostic{{Kind: "codex", Found: true, Version: "0.167.2"}}}).Hooks {
			switch row.Event {
			case "SessionStart":
				states.WriteString("[hooks.state.\"" + row.Key + "\"]\ntrusted_hash = \"" + expected[row.Event] + "\"\n")
			case "Stop":
				states.WriteString("[hooks.state.\"" + row.Key + "\"]\ntrusted_hash = \"sha256:" + strings.Repeat("0", 64) + "\"\n")
			case "PostToolUse":
				states.WriteString("[hooks.state.\"" + row.Key + "\"]\nenabled = false\n")
			}
		}
		_ = path
		writeCodexFixture(t, filepath.Join(root, "config.toml"), states.String())
	}
	record := func(target string) func(t *testing.T, home, state, root string, hooks []byte) {
		return func(t *testing.T, home, state, root string, hooks []byte) {
			writeCodexFixture(t, filepath.Join(state, "installation.json"), `{"schema":3,"home":`+jsonQuote(home)+`,"state":`+jsonQuote(state)+`,"targets":{".codex/hooks.json":`+jsonQuote(target)+`}}`)
		}
	}
	return []codexGoldenCase{
		{name: "compared_not_trusted", version: "0.167.2"},
		{name: "compared_with_sessions", version: "0.167.2", sessions: true},
		{name: "unverified_version", version: "0.167.3"},
		{name: "codex_not_found", version: "none"},
		{name: "mixed_trust_states", version: "0.167.2", setup: trust},
		{name: "hooks_file_missing", version: "0.167.2", setup: func(t *testing.T, _, _, root string, _ []byte) { _ = os.Remove(filepath.Join(root, "hooks.json")) }},
		{name: "hooks_file_invalid", version: "0.167.2", setup: func(t *testing.T, _, _, root string, _ []byte) {
			writeCodexFixture(t, filepath.Join(root, "hooks.json"), "{broken")
		}},
		{name: "config_toml_invalid", version: "0.167.2", setup: func(t *testing.T, _, _, root string, _ []byte) {
			writeCodexFixture(t, filepath.Join(root, "config.toml"), "= not toml")
		}},
		{name: "record_matches", version: "0.167.2", setup: func(t *testing.T, home, state, root string, hooks []byte) {
			record(filepath.Join(root, "hooks.json"))(t, home, state, root, hooks)
		}},
		{name: "record_other_target", version: "0.167.2", setup: func(t *testing.T, home, state, root string, hooks []byte) {
			record("/elsewhere/hooks.json")(t, home, state, root, hooks)
		}},
		{name: "record_invalid", version: "0.167.2", setup: func(t *testing.T, _, state, _ string, _ []byte) {
			writeCodexFixture(t, filepath.Join(state, "installation.json"), "{broken")
		}},
		{name: "record_other_schema", version: "0.167.2", setup: func(t *testing.T, home, state, _ string, _ []byte) {
			writeCodexFixture(t, filepath.Join(state, "installation.json"), `{"schema":2,"home":`+jsonQuote(home)+`,"state":`+jsonQuote(state)+`}`)
		}},
	}
}

func jsonQuote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func TestCodexDoctorSectionGolden(t *testing.T) {
	for _, test := range codexGoldenCases() {
		t.Run(test.name, func(t *testing.T) {
			for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
				t.Setenv(name, "")
			}
			home := codexDiagnosticTestDir(t)
			state := filepath.Join(home, "state")
			root := filepath.Join(home, ".codex")
			t.Setenv("CODEX_HOME", root)
			opts, _, hooks := newCodexDiagnosticFixture(t, home, state, root)
			if test.setup != nil {
				test.setup(t, home, state, root, hooks)
			}
			report := bus.DiagnosticReport{SessionScanComplete: true}
			if test.version != "none" {
				report.Agents = []bus.AgentDiagnostic{{Kind: "codex", Found: true, Version: test.version}}
			}
			if test.sessions {
				report.Sessions = []model.SessionDiagnostic{{Kind: "codex"}}
			}
			raw, err := json.MarshalIndent(codexDoctorSection(opts, report), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "doctor_"+test.name+".json", home, string(raw)+"\n")
		})
	}
}

// The unsafe-root cases: the diagnostic names no file and verifies nothing.
func TestCodexDoctorSectionGoldenUnsafeRoot(t *testing.T) {
	for _, root := range map[string]string{"relative": "relative", "control": "/tmp/bad\nroot"} {
		home := codexDiagnosticTestDir(t)
		t.Setenv("CODEX_HOME", root)
		opts, _, _ := newCodexDiagnosticFixture(t, home, filepath.Join(home, "state"), filepath.Join(home, ".codex"))
		t.Setenv("CODEX_HOME", root)
		report := bus.DiagnosticReport{Agents: []bus.AgentDiagnostic{{Kind: "codex", Found: true, Version: "0.167.2"}}}
		raw, err := json.MarshalIndent(codexDoctorSection(opts, report), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		checkGolden(t, "doctor_unsafe_root_"+map[bool]string{true: "control", false: "relative"}[strings.Contains(root, "\n")]+".json", home, string(raw)+"\n")
	}
}

// codexDoctorSection is what the doctor puts under "codex_hooks".
func codexDoctorSection(opts *commandOptions, report bus.DiagnosticReport) any {
	return diagnoseCodexHooks(opts, report)
}

func TestCodexInstallOutputGolden(t *testing.T) {
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	home := codexDiagnosticTestDir(t)
	state := filepath.Join(home, "state")
	steps := []struct {
		name string
		args []string
	}{
		{"install_first", []string{"install", "--apply", "--state", state}},
		{"install_second", []string{"install", "--apply", "--state", state}},
		{"install_drafts", []string{"install", "--out", filepath.Join(home, "drafts"), "--state", state}},
		{"uninstall", []string{"uninstall", "--apply", "--state", state}},
	}
	for _, step := range steps {
		out, errOut, err := execute(t, home, "", step.args...)
		if err != nil {
			t.Fatal(step.name, out, errOut, err)
		}
		checkGolden(t, "output_"+step.name+".txt", home, out)
	}
	t.Setenv("CODEX_HOME", "relative")
	out, errOut, err := execute(t, home, "", "install", "--apply", "--state", state)
	if err == nil {
		t.Fatal("a relative CODEX_HOME must fail the install")
	}
	checkGolden(t, "output_install_failed.txt", home, out+"\n--- stderr ---\n"+errOut+"\n--- error ---\n"+err.Error()+"\n")
}
