package main

import (
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

// adapterSections collects the doctor sections that adapters report about
// their own clients, keyed by the name each adapter gives its section. An
// adapter section never replaces a section the doctor already has.
func adapterSections(opts *commandOptions, report bus.DiagnosticReport, doctor map[string]any) {
	var recorded map[string]string
	var recordReason string
	read := false
	for _, kind := range opts.registry.Kinds() {
		adapter, err := opts.registry.Lookup(kind)
		if err != nil {
			continue
		}
		reporter, ok := adapter.(agent.DiagnosticsAdapter)
		if !ok {
			continue
		}
		state := opts.snapshot.Paths.StateDir.Value
		if !read {
			raw, readReason := readDiagnosticFile(filepath.Join(state, "installation.json"))
			recorded, recordReason = install.RecordedTargets(raw, readReason, opts.home, state)
			read = true
		}
		binary, _ := installationBinary() // an unknown binary leaves the adapter unable to verify
		input := agent.DiagnosticInput{
			Home: opts.home, StateDir: state, ConfigFile: opts.snapshot.Paths.ConfigFile.Value, Binary: binary,
			SessionScanComplete: report.SessionScanComplete, HasSessions: hasSessionOfKind(report, kind),
			RecordReason: recordReason, RecordedTargets: recorded,
		}
		input.Found, input.Version = clientVersion(report, kind)
		if key, section := reporter.Diagnose(input); key != "" {
			if _, exists := doctor[key]; !exists {
				doctor[key] = section
			}
		}
	}
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
// successful install, in adapter order.
func printInstallNotices(registry *agent.Registry, emit func(string) error) error {
	for _, kind := range registry.Kinds() {
		adapter, err := registry.Lookup(kind)
		if err != nil {
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
