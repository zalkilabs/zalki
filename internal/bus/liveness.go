package bus

import (
	"errors"
	"os"
	"syscall"
)

// Liveness is the evidence-based state of a session or recorded process.
// Unknown is not evidence of exit and must never be treated as stale by age.
type Liveness string

const (
	LivenessAlive   Liveness = "alive"
	LivenessExited  Liveness = "exited"
	LivenessUnknown Liveness = "unknown"
)

// LivenessView is the session evidence an adapter may use to decide liveness.
// It carries no credential.
type LivenessView struct {
	Endpoint *Endpoint
	Owner    *ProcessRef
}

// LivenessProbes are core's process and endpoint checks. Adapters must use
// these instead of inspecting the OS themselves.
type LivenessProbes struct {
	// Endpoint reports whether the recorded socket is still the same bound socket.
	Endpoint func(*Endpoint) error
	// Presence reports a process without a stored start-time fingerprint:
	// exited only when that pid is gone or a zombie, unknown when inspection
	// fails or the pid may have been reused.
	Presence func(pid int) Liveness
}

// LivenessAdapter replaces owner-process liveness. TracksOwnProcess is part of
// the same choice: a client that does not use the parent walk must not store it.
type LivenessAdapter interface {
	Liveness(LivenessView, LivenessProbes) Liveness
	TracksOwnProcess() bool
}

func (s Store) tracksOwnProcess(kind string) bool {
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return true
	}
	policy, ok := adapter.(LivenessAdapter)
	return !ok || policy.TracksOwnProcess()
}

func (s Store) sessionLiveness(v Session) Liveness {
	adapter, err := s.registry().Lookup(v.Kind)
	if err != nil {
		return LivenessUnknown
	}
	policy, ok := adapter.(LivenessAdapter)
	if !ok {
		// Adapters without a policy use the fingerprinted owner process.
		return processLiveness(v.Owner)
	}
	switch got := policy.Liveness(LivenessView{Endpoint: v.Endpoint, Owner: v.Owner}, s.livenessProbes()); got {
	case LivenessAlive, LivenessExited, LivenessUnknown:
		return got
	default:
		return LivenessUnknown
	}
}

func (s Store) livenessProbes() LivenessProbes {
	return LivenessProbes{
		Endpoint: CheckEndpoint,
		Presence: func(pid int) Liveness { return processPresence(pid, readProcess) },
	}
}

// processPresence is exit evidence without a start-time fingerprint. A live
// pid is not "alive": the pid may have been reused by an unrelated process.
func processPresence(pid int, read func(int) (processInfo, error)) Liveness {
	if pid <= 1 {
		return LivenessUnknown
	}
	current, err := read(pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return LivenessExited
	}
	if err != nil || current.Start == "" || !current.Zombie {
		return LivenessUnknown
	}
	return LivenessExited
}
