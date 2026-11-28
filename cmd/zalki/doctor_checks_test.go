package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func findCheck(t *testing.T, checks []doctorCheck, contains string) doctorCheck {
	t.Helper()
	for _, c := range checks {
		if strings.Contains(c.Name, contains) {
			return c
		}
	}
	t.Fatalf("no check with %q in %+v", contains, checks)
	return doctorCheck{}
}

func healthyFacts() doctorFacts {
	return doctorFacts{
		report: bus.DiagnosticReport{
			SessionScanComplete: true,
			Agents: []bus.AgentDiagnostic{
				{Kind: "claude", Label: "Claude", Found: true, Version: "2.1.0", Compatibility: "installed"},
				{Kind: "codex", Label: "Codex", Found: false, Compatibility: "not_installed"},
			},
			Notes: []string{"Process liveness does not establish an attached terminal.", "Check CODEX_HOME and HOME for the native profile.", "A Codex backend can outlive its terminal."},
		},
		background: doctorBackgroundView{EnabledPlugins: 2},
	}
}

func TestCoreChecksOfAHealthySetupHaveNoNoteAndNoProblem(t *testing.T) {
	checks := coreChecks(healthyFacts())
	if s := summarize(checks); s.Notes != 0 || s.Problems != 0 || s.OK != 5 {
		t.Fatalf("summary %+v for %+v", s, checks)
	}
	if c := findCheck(t, checks, "Codex"); c.Status != checkOK || !strings.Contains(c.Name, "not installed, skipped") {
		t.Fatalf("an absent client = %+v", c)
	}
	if c := findCheck(t, checks, "Claude"); c.Status != checkOK || !strings.Contains(c.Name, "version 2.1.0") {
		t.Fatalf("a present client = %+v", c)
	}
	if c := findCheck(t, checks, "No sessions"); c.Status != checkOK {
		t.Fatalf("sessions line = %+v", c)
	}
}

// The installation record matters only when a client is present, and only a
// missing or unusable record is a problem.
func TestCoreChecksInstallationRecord(t *testing.T) {
	for _, tc := range []struct {
		reason, status, fix string
	}{
		{"", checkOK, ""},
		{"missing_file", checkProblem, "zalki install"},
		{"invalid_data", checkProblem, "zalki upgrade"},
		{"unsafe_file", checkProblem, "zalki upgrade"},
		{"ambiguous_ownership", checkProblem, "zalki upgrade"},
	} {
		facts := healthyFacts()
		facts.recordReason = tc.reason
		c := findCheck(t, coreChecks(facts), "Zalki")
		if c.Status != tc.status || !strings.Contains(c.Fix, tc.fix) || (tc.status == checkProblem && c.Fix == "") {
			t.Fatalf("record %q = %+v", tc.reason, c)
		}
	}
	facts := healthyFacts()
	facts.report.Agents[0].Found = false
	facts.recordReason = "missing_file"
	if s := summarize(coreChecks(facts)); s.Problems != 0 {
		t.Fatalf("a missing record without any client is a problem: %+v", s)
	}
}

func TestCoreChecksSessionsRuntimesAndNotes(t *testing.T) {
	facts := healthyFacts()
	facts.report.Sessions = []model.SessionDiagnostic{
		{ID: "claude:1", Liveness: model.LivenessAlive, HookBuildStatus: "current"},
		{ID: "claude:2", Liveness: model.LivenessAlive, HookBuildStatus: "current"},
		{ID: "claude:3", Liveness: model.LivenessExited, HookBuildStatus: "outdated"},
		{ID: "claude:4", Retired: true, Liveness: model.LivenessExited, HookBuildStatus: "outdated"},
	}
	facts.report.UntrackedProcesses = []model.ProcessRef{{PID: 1}, {PID: 2}}
	facts.report.Runtimes = []bus.RuntimeInfo{{NeedsRestart: true}, {NeedsRestart: false}}
	facts.report.Notes = append(facts.report.Notes, "Skipped 2 unsupported runtime records; stop the old clients.")
	facts.background.Notes = []string{"background note"}
	facts.background.EnabledPlugins = -1
	checks := coreChecks(facts)
	for want, fix := range map[string]string{
		"1 session has exited": "zalki prune", "1 session runs an older Zalki build": "restart those client sessions",
		"1 running Zalki process uses an older build": "restart",
		"Skipped 2 unsupported runtime records":       "", "background note": "", "Plugin records cannot be read": "zalki plugins list",
	} {
		c := findCheck(t, checks, want)
		if c.Status != checkNote || (fix != "" && !strings.Contains(c.Fix, fix)) {
			t.Fatalf("%q = %+v", want, c)
		}
	}
	if c := findCheck(t, checks, "Sessions: 3 registered"); c.Status != checkOK {
		t.Fatalf("retired sessions are not counted: %+v", c)
	}
	if s := summarize(checks); s.Problems != 0 {
		t.Fatalf("notes must not be problems: %+v", s)
	}
	facts.report.Notes = append(facts.report.Notes, "OS process inspection unavailable: no access", "Process liveness does not establish an attached terminal.", "Check CODEX_HOME and HOME for the native profile.")
	for _, c := range coreChecks(facts) {
		if strings.Contains(c.Name, "Process liveness") || strings.Contains(c.Name, "CODEX_HOME") || strings.Contains(c.Name, "outlive") {
			t.Fatalf("an explanation became a finding: %+v", c)
		}
	}
	if c := findCheck(t, coreChecks(facts), "OS process inspection unavailable"); c.Status != checkNote {
		t.Fatalf("inspection note = %+v", c)
	}
}

// Untracked processes are in the JSON only: the user cannot act on the count.
func TestUntrackedProcessesAreNotAFinding(t *testing.T) {
	facts := healthyFacts()
	before := coreChecks(facts)
	facts.report.UntrackedProcesses = []model.ProcessRef{{PID: 1}, {PID: 2}}
	after := coreChecks(facts)
	if len(after) != len(before) {
		t.Fatalf("untracked processes changed the checklist: %+v", after)
	}
	for _, c := range after {
		if strings.Contains(c.Name, "tracked") {
			t.Fatalf("an untracked-process finding: %+v", c)
		}
	}
}

// The adapters' lines come right after the installation record, before the sessions.
func TestClientChecksAreListedNextToTheClientLines(t *testing.T) {
	facts := healthyFacts()
	facts.clientChecks = []doctorCheck{{Name: "Claude: Zalki settings are in place", Status: checkOK}}
	names := make([]string, 0, 8)
	for _, c := range coreChecks(facts) {
		names = append(names, c.Name)
	}
	want := []string{"Claude: found (version 2.1.0)", "Codex: not installed, skipped", "Zalki is set up for these clients", "Claude: Zalki settings are in place", "No sessions are registered yet"}
	if len(names) < len(want) || strings.Join(names[:len(want)], "|") != strings.Join(want, "|") {
		t.Fatalf("order = %q", names)
	}
}

func TestClientWithoutAVersionIsANoteWithTheHint(t *testing.T) {
	facts := healthyFacts()
	facts.report.Agents[0].Compatibility = "version_unavailable"
	facts.report.Agents[0].Version = ""
	facts.report.Agents[0].RecoveryHint = "Check the client's --version command."
	c := findCheck(t, coreChecks(facts), "Claude")
	if c.Status != checkNote || c.Fix != "Check the client's --version command." {
		t.Fatalf("no version = %+v", c)
	}
}

func TestAdapterChecksShowAnUnknownStatusAsAProblem(t *testing.T) {
	got := adapterChecks([]agent.Check{
		{Name: "a", Status: "ok"}, {Name: "b", Status: "note", Fix: "do b"}, {Name: "c", Status: "problem", Fix: "do c"},
		{Name: "d", Status: "prolbem"}, {Name: "e", Status: ""},
	})
	want := []string{checkOK, checkNote, checkProblem, checkProblem, checkProblem}
	for i, c := range got {
		if c.Status != want[i] {
			t.Fatalf("check %d = %+v, want %s", i, c, want[i])
		}
	}
}

func TestSummaryLineSaysWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		s    doctorSummary
		want string
	}{
		{doctorSummary{OK: 7}, "Doctor: 7 ok. No problems found."},
		{doctorSummary{OK: 7, Notes: 1}, "Doctor: 7 ok and 1 note. No problems found."},
		{doctorSummary{OK: 7, Notes: 2, Problems: 1}, "Doctor: 7 ok, 2 notes and 1 problem.\nNext: fix the problems marked above, then run zalki doctor again."},
		{doctorSummary{OK: 1, Problems: 2}, "Doctor: 1 ok and 2 problems.\nNext: fix the problems marked above, then run zalki doctor again."},
	} {
		if got := summaryLine(tc.s); got != tc.want {
			t.Fatalf("summary = %q, want %q", got, tc.want)
		}
	}
}

func TestPrintDoctorAlignsNamesAndPutsTheFixUnderTheLine(t *testing.T) {
	cmd, out, _ := portCommand()
	checks := []doctorCheck{
		{Name: "Claude: found (version 2)", Status: checkOK},
		{Name: "2 sessions have exited", Status: checkNote, Fix: "run zalki prune"},
		{Name: "Zalki is not set up", Status: checkProblem, Fix: "run zalki install"},
	}
	if err := printDoctor(cmd, checks, summarize(checks)); err != nil {
		t.Fatal(err)
	}
	want := "[ok]      Claude: found (version 2)\n" +
		"[note]    2 sessions have exited\n" +
		"          Next: run zalki prune\n" +
		"[problem] Zalki is not set up\n" +
		"          Next: run zalki install\n" +
		"\nDoctor: 1 ok, 1 note and 1 problem.\nNext: fix the problems marked above, then run zalki doctor again.\n"
	if out.String() != want {
		t.Fatalf("printed:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestErrReportedExitsWithoutPrintingAnything(t *testing.T) {
	var out bytes.Buffer
	printError(&out, errReported)
	printError(&out, errors.Join(errReported))
	if out.Len() != 0 || commandExitCode(errReported) != 1 {
		t.Fatalf("printed %q, exit %d", out.String(), commandExitCode(errReported))
	}
}

// A client's live sessions without a running listener get one note with the
// client's own recovery hint; exited or retired sessions and running listeners do not.
func TestListenerNotesAreOnePerClientWithItsOwnHint(t *testing.T) {
	facts := healthyFacts()
	facts.report.Agents = append(facts.report.Agents, bus.AgentDiagnostic{Kind: "cursor", Label: "Cursor", Found: true})
	facts.report.Sessions = []model.SessionDiagnostic{
		{ID: "cursor:1", Kind: "cursor", Liveness: model.LivenessAlive, ListenerHealth: "not_armed", RecoveryHint: "arm one listener"},
		{ID: "cursor:2", Kind: "cursor", Liveness: model.LivenessAlive, ListenerHealth: "stale", RecoveryHint: "arm one listener"},
		{ID: "cursor:3", Kind: "cursor", Liveness: model.LivenessAlive, ListenerHealth: "running"},
		{ID: "cursor:4", Kind: "cursor", Liveness: model.LivenessExited, ListenerHealth: "not_armed"},
		{ID: "cursor:5", Kind: "cursor", Retired: true, ListenerHealth: "not_armed"},
		{ID: "claude:1", Kind: "claude", Liveness: model.LivenessAlive},
	}
	var got []doctorCheck
	for _, c := range coreChecks(facts) {
		if strings.Contains(c.Name, "listener") {
			got = append(got, c)
		}
	}
	if len(got) != 1 || got[0].Name != "Cursor: 2 sessions have no running listener, so an idle chat is not woken" || got[0].Status != checkNote || got[0].Fix != "arm one listener" {
		t.Fatalf("listener notes = %+v", got)
	}
	facts.report.Sessions = facts.report.Sessions[:1]
	facts.report.Sessions[0].RecoveryHint = ""
	for _, c := range coreChecks(facts) {
		if strings.Contains(c.Name, "listener") && (c.Name != "Cursor: 1 session has no running listener, so an idle chat is not woken" || !strings.Contains(c.Fix, "zalki doctor --json")) {
			t.Fatalf("one session without a hint = %+v", c)
		}
	}
}
