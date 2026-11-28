package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/config"
)

// checkedStub is an adapter that has a section and its own checks.
type checkedStub struct{ secondStubSection }

func (checkedStub) Checks(agent.DiagnosticInput) []agent.Check {
	return []agent.Check{{Name: "stub hook", Status: "problem", Fix: "fix the stub"}, {Name: "stub tidy", Status: "ok"}}
}

func stubOpts(t *testing.T, adapters ...agent.Adapter) *commandOptions {
	t.Helper()
	registry, err := agent.NewRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	return &commandOptions{home: home, registry: registry, snapshot: &config.Snapshot{Paths: config.Paths{
		ConfigFile: config.Value[string]{Value: filepath.Join(home, "zalki.json")}, StateDir: config.Value[string]{Value: filepath.Join(home, "state")},
	}}}
}

// An adapter with checks adds its lines; one without adds nothing here (the
// settings check speaks for every client).
func TestAdapterSectionsGiveTheChecklistOfAdaptersThatHaveOne(t *testing.T) {
	opts := stubOpts(t, stubSectionAdapter{}, checkedStub{})
	report := bus.DiagnosticReport{Agents: []bus.AgentDiagnostic{
		{Kind: "stubsection", Label: "Stub", Found: true}, {Kind: "stubsecond", Label: "Second", Found: true},
	}}
	checks := adapterSections(opts, report, map[string]any{}, nil, "")
	if len(checks) != 2 {
		t.Fatalf("checks = %+v", checks)
	}
	if c := findCheck(t, checks, "stub hook"); c.Status != checkProblem || c.Fix != "fix the stub" {
		t.Fatalf("adapter problem = %+v", c)
	}
}

// The checks and the summary keys belong to the doctor: no adapter section can take them.
func TestAnAdapterSectionCannotTakeTheChecksKey(t *testing.T) {
	opts := stubOpts(t, keyStub{}, summaryStub{})
	doctor := newDoctorOutput("state", bus.DiagnosticReport{}, doctorBackgroundView{})
	adapterSections(opts, bus.DiagnosticReport{}, doctor, nil, "")
	if _, ok := doctor["checks"].([]doctorCheck); !ok {
		t.Fatalf("an adapter section took the checks key: %v", doctor["checks"])
	}
	if _, ok := doctor["summary"].(doctorSummary); !ok {
		t.Fatalf("an adapter section took the summary key: %v", doctor["summary"])
	}
}

type keyStub struct{ stubSectionAdapter }

func (keyStub) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "keystub", Label: "Key"}
}
func (keyStub) Diagnose(agent.DiagnosticInput) (string, any) { return "checks", "taken" }

type summaryStub struct{ stubSectionAdapter }

func (summaryStub) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "summarystub", Label: "Summary"}
}
func (summaryStub) Diagnose(agent.DiagnosticInput) (string, any) { return "summary", "taken" }

func doctorWithFakeCodex(t *testing.T) (home, state string) {
	t.Helper()
	home = codexDiagnosticTestDir(t)
	state = filepath.Join(home, "state")
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	fake := filepath.Join(home, "bin", "codex")
	writeCodexFixture(t, fake, "#!/bin/sh\nprintf 'codex-cli 0.167.2\\n'\n")
	if err := os.Chmod(fake, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))
	return home, state
}

// A present client with no installation record is a problem: the words come
// first, then the exit code; the JSON keeps its old keys and gains two.
func TestDoctorWordsAndJSONForAClientThatIsNotSetUp(t *testing.T) {
	home, state := doctorWithFakeCodex(t)
	out, diagnostic, err := execute(t, home, "", "doctor", "--state", state)
	if !errors.Is(err, errReported) || diagnostic != "" {
		t.Fatalf("exit %v stderr %q", err, diagnostic)
	}
	for _, want := range []string{"[ok]      Codex: found", "[problem] Zalki is not set up for the clients above\n          Next: run zalki install, then zalki doctor again\n", "Doctor: ", "problem.\nNext: fix the problems marked above"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "{") {
		t.Fatalf("JSON in the words output:\n%s", out)
	}
	raw, _, err := execute(t, home, "", "doctor", "--json", "--state", state)
	if !errors.Is(err, errReported) {
		t.Fatalf("--json exit %v", err)
	}
	var report map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &report) != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	for _, key := range []string{"state", "agents", "runtime", "runtimes", "untracked_processes", "sessions", "notes", "plugins_background", "checks", "summary", "codex_hooks"} {
		if _, ok := report[key]; !ok {
			t.Fatalf("doctor JSON lost key %q", key)
		}
	}
	var summary doctorSummary
	if json.Unmarshal(report["summary"], &summary) != nil || summary.Problems < 1 {
		t.Fatalf("summary %s", report["summary"])
	}
}

// With no client present nothing can be broken: exit 0.
func TestDoctorWithoutClientsHasNoProblem(t *testing.T) {
	home := codexDiagnosticTestDir(t)
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	t.Setenv("PATH", filepath.Join(home, "empty"))
	out, diagnostic, err := execute(t, home, "", "doctor", "--state", filepath.Join(home, "state"))
	if err != nil || diagnostic != "" || !strings.Contains(out, "not installed, skipped") || !strings.Contains(out, "No problems found.") || strings.Contains(out, "[problem]") {
		t.Fatalf("exit %v stderr %q out:\n%s", err, diagnostic, out)
	}
}

// After install the record exists and that problem is gone.
func TestDoctorSeesTheInstallationRecordAfterInstall(t *testing.T) {
	home, state := doctorWithFakeCodex(t)
	if _, _, err := execute(t, home, "", "install", "--yes", "--state", state); err != nil {
		t.Fatal(err)
	}
	out, _, err := execute(t, home, "", "doctor", "--state", state)
	if strings.Contains(out, "Zalki is not set up") || !strings.Contains(out, "[ok]      Zalki is set up for these clients") {
		t.Fatalf("exit %v out:\n%s", err, out)
	}
}

func TestRunDoctorFailureHidesTheCauseAndKeepsTheUser(t *testing.T) {
	fake := &fakeDiagnostics{diagErr: errors.New("/private/path broke")}
	cmd, out, _ := portCommand()
	cmd.SetContext(context.Background())
	err := runDoctor(cmd, stubOpts(t), fake, bus.Store{}, false)
	if !errors.Is(err, fake.diagErr) || out.Len() != 0 || strings.Contains(errorLines(err), "private") || !strings.HasPrefix(errorLines(err), "Failed: ") {
		t.Fatalf("error %v out %q", err, out.String())
	}
}

// doctorExitOK: doctor exits with errReported when it found a problem; tests that
// read its output accept that, whatever clients the machine has.
func doctorExitOK(err error) bool { return err == nil || errors.Is(err, errReported) }
