package install

import (
	"errors"
	"path/filepath"
)

// Internals exposed to the external install_test package.
type Installation = installation

func LockInstallation(home string) (func(), error) { return lockInstallation(home) }

// PlannedEdits reports how many file edits a request would make, without writing.
func PlannedEdits(declare Declare, request Request) (int, error) {
	plan, err := declare.plan(request)
	return len(plan.edits), err
}

// CrashAfter plans request, writes the pending record and the first n client
// edits, and stops there as a crash would. It returns the number of client edits.
func CrashAfter(declare Declare, request Request, n int) (int, error) {
	plan, err := declare.plan(request)
	if err != nil {
		return 0, err
	}
	clients := plan.edits
	if last := len(clients) - 1; last >= 0 && filepath.Base(clients[last].path) == "installation.json" {
		clients = clients[:last]
	}
	if n < 0 {
		n = len(clients)
	}
	if err := errors.Join(prepareConfigEdits(clients[:n]), plan.writePending()); err != nil {
		return 0, err
	}
	for _, edit := range clients[:n] {
		if err := writeConfigEdit(edit); err != nil {
			return 0, err
		}
	}
	return len(clients), nil
}

var (
	AddMCPFragment            = addMCPFragment
	ValidateInstallationPatch = validateInstallationPatch
)

// RemoveMCPFragment removes the zalki table and returns the new bytes only.
func RemoveMCPFragment(before []byte, fragments ...string) ([]byte, error) {
	after, _, err := removeMCPFragment(before, "", fragments...)
	return after, err
}

// RemoveRecordedHooks exposes the hook removal for unit tests. It returns the
// result sentences of the notes that name the changed keys.
func RemoveRecordedHooks(current map[string]any, patches []map[string]any, uninstall bool) ([]string, error) {
	removed, err := removeRecordedHooks(current, patches, uninstall)
	notes := hookNotes("PATH", removed)
	results := make([]string, 0, len(notes))
	for _, note := range notes {
		results = append(results, note.result)
	}
	return results, err
}
