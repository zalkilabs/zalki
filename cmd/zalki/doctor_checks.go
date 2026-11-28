package main

import (
	"fmt"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// The doctor checklist: one line per finding. A problem means messaging
// cannot work for a client that is present; a note is advice. The JSON keeps
// every field it had; the list is added as "checks" and "summary".
const (
	checkOK      = "ok"
	checkNote    = "note"
	checkProblem = "problem"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Fix    string `json:"fix,omitempty"`
}

type doctorSummary struct {
	OK       int `json:"ok"`
	Notes    int `json:"notes"`
	Problems int `json:"problems"`
}

// The report's notes mix explanations that every report has (they stay in the
// JSON) with findings. Only these two kinds are findings: records that were
// skipped, and process inspection that is not available.
func isFindingNote(note string) bool {
	return strings.HasPrefix(note, "Skipped ") || strings.HasPrefix(note, "OS process inspection unavailable")
}

// doctorFacts is the plain data the core checks read.
type doctorFacts struct {
	report       bus.DiagnosticReport
	recordReason string // "": the installation record is usable
	background   doctorBackgroundView
	// clientChecks are the lines about the clients' settings and their adapters;
	// they are listed right after the installation record, next to the client lines.
	clientChecks []doctorCheck
}

// coreChecks lists the findings that do not belong to one client's adapter.
func coreChecks(f doctorFacts) []doctorCheck {
	var checks []doctorCheck
	found := 0
	for _, client := range f.report.Agents {
		check, present := clientCheck(client)
		checks = append(checks, check)
		if present {
			found++
		}
	}
	if found > 0 {
		checks = append(checks, recordCheck(f.recordReason))
	}
	checks = append(checks, f.clientChecks...)
	checks = append(checks, sessionChecks(f.report)...)
	checks = append(checks, runtimeChecks(f.report)...)
	for _, note := range f.report.Notes {
		if isFindingNote(note) {
			checks = append(checks, doctorCheck{Name: plainText(note), Status: checkNote})
		}
	}
	return append(checks, pluginChecks(f.background)...)
}

// anyClientFound says whether a supported client is on this machine.
func anyClientFound(report bus.DiagnosticReport) bool {
	for _, client := range report.Agents {
		if client.Found {
			return true
		}
	}
	return false
}

func clientCheck(client bus.AgentDiagnostic) (doctorCheck, bool) {
	switch {
	case !client.Found:
		return doctorCheck{Name: client.Label + ": not installed, skipped", Status: checkOK}, false
	case client.Compatibility == "version_unavailable":
		return doctorCheck{Name: client.Label + ": found, but its version cannot be read", Status: checkNote, Fix: orDefault(client.RecoveryHint, "check that the client starts with --version")}, true
	case client.Version != "":
		return doctorCheck{Name: fmt.Sprintf("%s: found (version %s)", client.Label, plainText(client.Version)), Status: checkOK}, true
	}
	return doctorCheck{Name: client.Label + ": found", Status: checkOK}, true
}

func recordCheck(reason string) doctorCheck {
	switch reason {
	case "":
		return doctorCheck{Name: "Zalki installation found", Status: checkOK}
	case "missing_file":
		return doctorCheck{Name: "Zalki is not set up for the clients above", Status: checkProblem, Fix: "run zalki install, then zalki doctor again"}
	}
	return doctorCheck{Name: "Zalki's installation record cannot be used", Status: checkProblem, Fix: "run zalki upgrade, then zalki doctor again"}
}

func sessionChecks(report bus.DiagnosticReport) []doctorCheck {
	var live, exited, outdated int
	for _, s := range report.Sessions {
		if s.Retired {
			continue
		}
		live++
		if s.Liveness == model.LivenessExited {
			exited++
		}
		if s.HookBuildStatus == "outdated" {
			outdated++
		}
	}
	checks := []doctorCheck{{Name: sessionsLine(live), Status: checkOK}}
	checks = append(checks, listenerChecks(report)...)
	if exited > 0 {
		checks = append(checks, doctorCheck{Name: countOf(exited, "session has", "sessions have") + " exited", Status: checkNote, Fix: "run zalki prune"})
	}
	if outdated > 0 {
		checks = append(checks, doctorCheck{Name: countOf(outdated, "session runs", "sessions run") + " an older Zalki build", Status: checkNote, Fix: "restart those client sessions"})
	}
	return checks
}

// listenerChecks is one note per client whose live sessions have no running
// listener: they still get messages at their next step, but an idle chat is not
// woken. The fix is the client's own recovery hint for such a session.
func listenerChecks(report bus.DiagnosticReport) []doctorCheck {
	type group struct {
		n    int
		hint string
	}
	byKind := map[string]*group{}
	var order []string
	for _, s := range report.Sessions {
		if s.Retired || s.Liveness == model.LivenessExited {
			continue
		}
		switch s.ListenerHealth {
		case "stale", "not_armed", "legacy_running":
		default:
			continue
		}
		g := byKind[s.Kind]
		if g == nil {
			g = &group{hint: s.RecoveryHint}
			byKind[s.Kind] = g
			order = append(order, s.Kind)
		}
		g.n++
	}
	checks := make([]doctorCheck, 0, len(order))
	for _, kind := range order {
		g := byKind[kind]
		label := kind
		for _, client := range report.Agents {
			if client.Kind == kind && client.Label != "" {
				label = client.Label
			}
		}
		fix := orDefault(plainText(g.hint), "see the recovery hint of each session in zalki doctor --json")
		name := label + ": " + countOf(g.n, "session has", "sessions have") + " no running listener, so an idle chat is not woken"
		checks = append(checks, doctorCheck{Name: name, Status: checkNote, Fix: fix})
	}
	return checks
}

func sessionsLine(live int) string {
	if live == 0 {
		return "No sessions are registered yet"
	}
	return "Sessions: " + countOf(live, "registered", "registered")
}

func runtimeChecks(report bus.DiagnosticReport) []doctorCheck {
	stale := 0
	for _, r := range report.Runtimes {
		if r.NeedsRestart {
			stale++
		}
	}
	if stale == 0 {
		return nil
	}
	return []doctorCheck{{Name: countOf(stale, "running Zalki process uses", "running Zalki processes use") + " an older build", Status: checkNote, Fix: "restart the client sessions that started them"}}
}

func pluginChecks(background doctorBackgroundView) []doctorCheck {
	var checks []doctorCheck
	if background.EnabledPlugins < 0 {
		checks = append(checks, doctorCheck{Name: "Plugin records cannot be read", Status: checkNote, Fix: "run zalki plugins list"})
	} else {
		checks = append(checks, doctorCheck{Name: "Plugins: " + countOf(background.EnabledPlugins, "enabled", "enabled"), Status: checkOK})
	}
	for _, note := range background.Notes {
		checks = append(checks, doctorCheck{Name: plainText(note), Status: checkNote})
	}
	return checks
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// adapterChecks turns an adapter's checks into doctor lines. A status that is
// not ok or note is shown as a problem: a typo must never hide a finding.
func adapterChecks(checks []agent.Check) []doctorCheck {
	out := make([]doctorCheck, 0, len(checks))
	for _, c := range checks {
		status := c.Status
		if status != checkOK && status != checkNote {
			status = checkProblem
		}
		out = append(out, doctorCheck{Name: plainText(c.Name), Status: status, Fix: plainText(c.Fix)})
	}
	return out
}

func summarize(checks []doctorCheck) doctorSummary {
	var s doctorSummary
	for _, c := range checks {
		switch c.Status {
		case checkOK:
			s.OK++
		case checkNote:
			s.Notes++
		default:
			s.Problems++
		}
	}
	return s
}
