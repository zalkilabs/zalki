package main

import (
	"slices"
	"strings"

	"github.com/zalkilabs/zalki/internal/install"
)

// settingsChecks compares the clients' settings files with what Zalki would
// write, using the installer's own preview of an upgrade. The preview reads
// the files and the installation record and writes nothing.
func settingsChecks(opts *commandOptions) []doctorCheck {
	binary, err := installationBinary()
	if err != nil {
		return []doctorCheck{{Name: "Zalki cannot find its own program, so the client settings cannot be checked", Status: checkProblem, Fix: "run zalki from the place it was installed, then run zalki doctor again"}}
	}
	report, err := manageConfiguration(opts, binary, false)
	return settingsFindings(report, err, func(kind string) string { return kindLabel(opts, kind) })
}

// settingsFindings turns the preview of an upgrade into doctor lines: a client
// whose file cannot be used and the files that differ from Zalki's are
// problems, a client with nothing to change is in place.
func settingsFindings(report install.Report, err error, label func(kind string) string) []doctorCheck {
	var checks []doctorCheck
	if err != nil {
		return []doctorCheck{{Name: "Zalki cannot check the client settings: " + plainText(err.Error()), Status: checkProblem, Fix: "fix what the message names, then run zalki doctor again"}}
	}
	for _, name := range report.Unusable {
		checks = append(checks, doctorCheck{Name: name + ": its settings file cannot be used" + skipReason(report.Notes, name), Status: checkProblem, Fix: "fix that file, then run zalki upgrade"})
	}
	outdated, unnamed := map[string]bool{}, 0
	for _, path := range report.Paths {
		if name, ok := report.Clients[path]; ok {
			outdated[name] = true
		} else {
			unnamed++
		}
	}
	for _, kind := range report.Configured {
		// A client with a differing file is not in place; with a file that has no
		// client name nobody can be called in place.
		if unnamed > 0 || outdated[label(kind)] {
			continue
		}
		checks = append(checks, doctorCheck{Name: label(kind) + ": Zalki settings are in place", Status: checkOK})
	}
	if len(report.Paths) > 0 && len(report.Configured) > 0 {
		checks = append(checks, outOfDateChecks(report)...)
	}
	return checks
}

// outOfDateChecks says, per client, how many of its settings files differ from
// what Zalki would write. The installer names the client of each file
// (Report.Clients); files with no client get one line of their own.
func outOfDateChecks(report install.Report) []doctorCheck {
	perClient, unnamed := map[string]int{}, 0
	for _, path := range report.Paths {
		if name, ok := report.Clients[path]; ok {
			perClient[name]++
		} else {
			unnamed++
		}
	}
	names := make([]string, 0, len(perClient))
	for name := range perClient {
		names = append(names, name)
	}
	slices.Sort(names)
	checks := make([]doctorCheck, 0, len(names)+1)
	for _, name := range names {
		checks = append(checks, doctorCheck{Name: name + ": " + countOf(perClient[name], "settings file is", "settings files are") + " out of date", Status: checkProblem, Fix: "run zalki upgrade"})
	}
	if unnamed > 0 {
		checks = append(checks, doctorCheck{Name: "Zalki's settings are out of date (" + countOf(unnamed, "file", "files") + ")", Status: checkProblem, Fix: "run zalki upgrade"})
	}
	return checks
}

// skipReason is " (reason)" from the installer's "Skipped NAME: reason." line.
func skipReason(notes []string, name string) string {
	prefix := "Skipped " + name + ": "
	for _, note := range notes {
		if reason, ok := strings.CutPrefix(note, prefix); ok {
			return " (" + plainText(strings.TrimSuffix(reason, ".")) + ")"
		}
	}
	return ""
}

// kindLabel is the display name of a client kind.
func kindLabel(opts *commandOptions, kind string) string {
	if adapter, err := opts.registry.Lookup(kind); err == nil {
		if label := adapter.Descriptor().Label; label != "" {
			return label
		}
	}
	return kind
}
