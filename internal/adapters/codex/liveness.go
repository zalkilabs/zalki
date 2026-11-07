package codex

import (
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
)

// TracksOwnProcess reports that the parent walk is not one Codex thread.
// Hooks identify the shared app-server daemon, which outlives every thread.
func (Adapter) TracksOwnProcess() bool { return false }

// Liveness stays unknown. Codex hooks carry no per-thread process or socket,
// and the app-server control socket belongs to every thread. Calling that
// daemon alive would mark idle threads live; calling it exited would refuse
// sends and prune threads that are only quiet. Unknown keeps both available.
func (Adapter) Liveness(agent.LivenessView, agent.LivenessProbes) bus.Liveness {
	return bus.LivenessUnknown
}
