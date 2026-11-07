package codex

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestCodexLivenessIgnoresTheSharedDaemon(t *testing.T) {
	daemon := &bus.ProcessRef{PID: 71228, Start: "daemon", Role: "backend"}
	probes := agent.LivenessProbes{
		Endpoint: func(*bus.Endpoint) error { t.Fatal("Codex liveness inspected a shared socket"); return nil },
		Presence: func(int) bus.Liveness { t.Fatal("Codex liveness inspected a shared process"); return bus.LivenessAlive },
	}
	for _, owner := range []*bus.ProcessRef{daemon, nil} {
		if got := (Adapter{}).Liveness(agent.LivenessView{Owner: owner}, probes); got != "unknown" {
			t.Fatalf("shared daemon liveness was %s", got)
		}
	}
	if (Adapter{}).TracksOwnProcess() {
		t.Fatal("Codex recorded the shared app-server as a thread process")
	}
}
