package main

import (
	"path/filepath"
	"slices"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

// readInstallRecord reads the installation record once: the installed files
// it names and why it cannot be used ("" when it can).
func readInstallRecord(opts *commandOptions) (recorded map[string]string, reason string) {
	state := opts.snapshot.Paths.StateDir.Value
	raw, readReason := readDiagnosticFile(filepath.Join(state, "installation.json"))
	return install.RecordedTargets(raw, readReason, opts.home, state)
}

// adapterSections collects the doctor sections that adapters report about
// their own clients, keyed by the name each adapter gives its section, and the
// checklist lines of the adapters that have them. An adapter section never
// replaces a section the doctor already has. A present client whose adapter
// has no checks gets "set up" when the installation record is usable; without
// a usable record the record problem already says it.
func adapterSections(opts *commandOptions, report bus.DiagnosticReport, doctor map[string]any, recorded map[string]string, recordReason string) []doctorCheck {
	var checks []doctorCheck
	for _, kind := range opts.registry.Kinds() {
		adapter, err := opts.registry.Lookup(kind)
		if err != nil {
			continue
		}
		state := opts.snapshot.Paths.StateDir.Value
		binary, _ := installationBinary() // an unknown binary leaves the adapter unable to verify
		input := agent.DiagnosticInput{
			Home: opts.home, StateDir: state, ConfigFile: opts.snapshot.Paths.ConfigFile.Value, Binary: binary,
			SessionScanComplete: report.SessionScanComplete, HasSessions: hasSessionOfKind(report, kind),
			RecordReason: recordReason, RecordedTargets: recorded,
		}
		input.Found, input.Version = clientVersion(report, kind)
		if reporter, ok := adapter.(agent.DiagnosticsAdapter); ok {
			if key, section := reporter.Diagnose(input); key != "" {
				if _, exists := doctor[key]; !exists {
					doctor[key] = section
				}
			}
		}
		if checker, ok := adapter.(agent.CheckAdapter); ok {
			checks = append(checks, adapterChecks(checker.Checks(input))...)
		} else if input.Found && recordReason == "" {
			// No checks of its own yet: the usable installation record is all that is known.
			checks = append(checks, doctorCheck{Name: clientLabel(report, kind) + ": set up", Status: checkOK})
		}
	}
	return checks
}

// clientLabel is the name the doctor shows for a client.
func clientLabel(report bus.DiagnosticReport, kind string) string {
	for _, client := range report.Agents {
		if client.Kind == kind {
			return client.Label
		}
	}
	return kind
}

// clientVersion gives the doctor's finding for one adapter's client.
func clientVersion(report bus.DiagnosticReport, kind string) (found bool, version string) {
	for _, client := range report.Agents {
		if client.Kind == kind {
			return client.Found, client.Version
		}
	}
	return false, ""
}

// hasSessionOfKind says whether any session record belongs to the adapter.
func hasSessionOfKind(report bus.DiagnosticReport, kind string) bool {
	for _, session := range report.Sessions {
		if session.Kind == kind {
			return true
		}
	}
	return false
}

// printInstallNotices prints the line each adapter wants the user to see after a
// successful install, in adapter order. Only the lines of the clients the installer
// configured are printed.
func printInstallNotices(registry *agent.Registry, configured []string, emit func(string) error) error {
	for _, kind := range registry.Kinds() {
		adapter, err := registry.Lookup(kind)
		if err != nil || !slices.Contains(configured, kind) {
			continue
		}
		if notice := adapter.Descriptor().InstallNotice; notice != "" {
			if err := emit(notice); err != nil {
				return err
			}
		}
	}
	return nil
}
