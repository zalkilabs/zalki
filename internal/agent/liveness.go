package agent

import "github.com/zalkilabs/zalki/internal/model"

// LivenessView is the session evidence an adapter may use to decide liveness.
// It carries no credential.
type LivenessView struct {
	Endpoint *model.Endpoint
	Owner    *model.ProcessRef
}

// LivenessProbes are core's process and endpoint checks. Adapters must use
// these instead of inspecting the OS themselves.
type LivenessProbes struct {
	// Endpoint reports whether the recorded socket is still the same bound socket.
	Endpoint func(*model.Endpoint) error
	// Presence reports a process without a stored start-time fingerprint:
	// exited only when that pid is gone or a zombie, unknown when inspection
	// fails or the pid may have been reused.
	Presence func(pid int) model.Liveness
}

// LivenessAdapter replaces owner-process liveness. TracksOwnProcess is part of
// the same choice: a client that does not use the parent walk must not store it.
type LivenessAdapter interface {
	Liveness(LivenessView, LivenessProbes) model.Liveness
	TracksOwnProcess() bool
}
