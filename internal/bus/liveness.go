package bus

import (
	"errors"
	"os"
	"syscall"
)

const (
	livenessAlive   = "alive"
	livenessExited  = "exited"
	livenessUnknown = "unknown"
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
	Presence func(pid int) string
}

// livenessAdapter replaces owner-process liveness. TracksOwnProcess is part of
// the same choice: a client that does not use the parent walk must not store it.
type livenessAdapter interface {
	Liveness(LivenessView, LivenessProbes) string
	TracksOwnProcess() bool
}

func (s Store) tracksOwnProcess(kind string) bool {
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return true
	}
	policy, ok := adapter.(livenessAdapter)
	return !ok || policy.TracksOwnProcess()
}

func (s Store) sessionLiveness(v Session) string {
	adapter, err := s.registry().Lookup(v.Kind)
	if err != nil {
		return livenessUnknown
	}
	policy, ok := adapter.(livenessAdapter)
	if !ok {
		return processLiveness(v.Owner)
	}
	switch got := policy.Liveness(LivenessView{Endpoint: v.Endpoint, Owner: v.Owner}, s.livenessProbes()); got {
	case livenessAlive, livenessExited, livenessUnknown:
		return got
	default:
		return livenessUnknown
	}
}

func (s Store) livenessProbes() LivenessProbes {
	return LivenessProbes{
		Endpoint: CheckEndpoint,
		Presence: func(pid int) string { return processPresence(pid, readProcess) },
	}
}

// processPresence is exit evidence without a start-time fingerprint. A live
// pid is not "alive": the pid may have been reused by an unrelated process.
func processPresence(pid int, read func(int) (processInfo, error)) string {
	if pid <= 1 {
		return livenessUnknown
	}
	current, err := read(pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return livenessExited
	}
	if err != nil || current.Start == "" || !current.Zombie {
		return livenessUnknown
	}
	return livenessExited
}
