package claude

import (
	"errors"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestClaudeLivenessUsesTheSessionSocket(t *testing.T) {
	endpoint := &model.Endpoint{Path: "/tmp/cc-socks/12076.sock", Device: 1, Inode: 2}
	helper := &model.ProcessRef{PID: 87443, Start: "helper", Role: "client"}
	bound := agent.LivenessProbes{
		Endpoint: func(*model.Endpoint) error { return nil },
		Presence: func(int) model.Liveness { return model.LivenessUnknown },
	}
	if got := (Adapter{}).Liveness(agent.LivenessView{Endpoint: endpoint, Owner: helper}, bound); got != "alive" {
		t.Fatalf("a bound session socket was %s", got)
	}
	crashed := agent.LivenessProbes{
		Endpoint: func(*model.Endpoint) error { return nil },
		Presence: func(pid int) model.Liveness {
			if pid != 12076 {
				t.Fatalf("probed pid %d", pid)
			}
			return model.LivenessExited
		},
	}
	if got := (Adapter{}).Liveness(agent.LivenessView{Endpoint: endpoint, Owner: helper}, crashed); got != "exited" {
		t.Fatalf("a leftover socket for a dead process was %s", got)
	}

	missing := agent.LivenessProbes{
		Endpoint: func(*model.Endpoint) error { return errors.New("gone") },
		Presence: func(pid int) model.Liveness {
			if pid != 12076 {
				t.Fatalf("probed pid %d", pid)
			}
			return model.LivenessExited
		},
	}
	if got := (Adapter{}).Liveness(agent.LivenessView{Endpoint: endpoint, Owner: helper}, missing); got != "exited" {
		t.Fatalf("a gone session pid was %s", got)
	}

	reused := agent.LivenessProbes{
		Endpoint: func(*model.Endpoint) error { return errors.New("replaced") },
		Presence: func(int) model.Liveness { return model.LivenessUnknown },
	}
	if got := (Adapter{}).Liveness(agent.LivenessView{Endpoint: endpoint, Owner: helper}, reused); got != "unknown" {
		t.Fatalf("an unfingerprinted pid was %s", got)
	}
	if got := (Adapter{}).Liveness(agent.LivenessView{Owner: helper}, reused); got != "unknown" {
		t.Fatalf("a helper owner without a socket was %s", got)
	}
	if (Adapter{}).TracksOwnProcess() {
		t.Fatal("Claude recorded a shared helper as the session process")
	}
}
