package claude

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
)

// TracksOwnProcess reports that the parent walk is not this session. Claude
// Code starts hooks from a helper that several sessions share.
func (Adapter) TracksOwnProcess() bool { return false }

// Liveness treats the recorded messaging socket as this session. A crash can
// leave the socket file in place, so a gone pid is exited before a matching
// inode counts as alive. A live pid without a stored start time may have been
// reused, so it is not alive by itself.
func (Adapter) Liveness(v bus.LivenessView, probes bus.LivenessProbes) string {
	if pid, ok := messagingSocketPID(v.Endpoint); ok && probes.Presence != nil && probes.Presence(pid) == "exited" {
		return "exited"
	}
	if v.Endpoint != nil && probes.Endpoint != nil && probes.Endpoint(v.Endpoint) == nil {
		return "alive"
	}
	return "unknown"
}

func messagingSocketPID(endpoint *bus.Endpoint) (int, bool) {
	if endpoint == nil {
		return 0, false
	}
	pidText, ok := strings.CutSuffix(filepath.Base(endpoint.Path), ".sock")
	if !ok || pidText == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 1 {
		return 0, false
	}
	return pid, true
}
