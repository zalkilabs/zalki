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
	RemoveMCPFragment         = removeMCPFragment
	ValidateInstallationPatch = validateInstallationPatch
)
