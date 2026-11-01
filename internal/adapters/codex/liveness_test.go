package codex

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestCodexLivenessIgnoresTheSharedDaemon(t *testing.T) {
	daemon := &bus.ProcessRef{PID: 71228, Start: "daemon", Role: "backend"}
	probes := bus.LivenessProbes{
		Endpoint: func(*bus.Endpoint) error { t.Fatal("Codex liveness inspected a shared socket"); return nil },
		Presence: func(int) string { t.Fatal("Codex liveness inspected a shared process"); return "alive" },
	}
	for _, owner := range []*bus.ProcessRef{daemon, nil} {
		if got := (Adapter{}).Liveness(bus.LivenessView{Owner: owner}, probes); got != "unknown" {
			t.Fatalf("shared daemon liveness was %s", got)
		}
	}
	if (Adapter{}).TracksOwnProcess() {
		t.Fatal("Codex recorded the shared app-server as a thread process")
	}
}
