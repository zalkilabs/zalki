package bus

import (
	"errors"
	"os"
	"syscall"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

func (s Store) tracksOwnProcess(kind string) bool {
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return true
	}
	policy, ok := adapter.(agent.LivenessAdapter)
	return !ok || policy.TracksOwnProcess()
}

func (s Store) sessionLiveness(v Session) model.Liveness {
	adapter, err := s.registry().Lookup(v.Kind)
	if err != nil {
		return model.LivenessUnknown
	}
	policy, ok := adapter.(agent.LivenessAdapter)
	if !ok {
		// Adapters without a policy use the fingerprinted owner process.
		return processLiveness(v.Owner)
	}
	switch got := policy.Liveness(agent.LivenessView{Endpoint: v.Endpoint, Owner: v.Owner}, s.livenessProbes()); got {
	case model.LivenessAlive, model.LivenessExited, model.LivenessUnknown:
		return got
	default:
		return model.LivenessUnknown
	}
}

func (s Store) livenessProbes() agent.LivenessProbes {
	return agent.LivenessProbes{
		Endpoint: CheckEndpoint,
		Presence: func(pid int) model.Liveness { return processPresence(pid, procwatch.Read) },
	}
}

// processPresence is exit evidence without a start-time fingerprint. A live
// pid is not "alive": the pid may have been reused by an unrelated process.
func processPresence(pid int, read func(int) (procwatch.Info, error)) model.Liveness {
	if pid <= 1 {
		return model.LivenessUnknown
	}
	current, err := read(pid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return model.LivenessExited
	}
	if err != nil || current.Start == "" || !current.Zombie {
		return model.LivenessUnknown
	}
	return model.LivenessExited
}
