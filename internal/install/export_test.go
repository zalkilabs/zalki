package install

// Internals exposed to the external install_test package.
type Installation = installation

func LockInstallation(home string) (func(), error) { return lockInstallation(home) }

// PlannedEdits reports how many file edits a request would make, without writing.
func PlannedEdits(declare Declare, request Request) (int, error) {
	plan, err := declare.plan(request)
	return len(plan.edits), err
}

var (
	AddMCPFragment            = addMCPFragment
	RemoveMCPFragment         = removeMCPFragment
	ValidateInstallationPatch = validateInstallationPatch
)
