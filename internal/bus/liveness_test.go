package bus

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type livenessStub struct {
	stubCursorAdapter
	result model.Liveness
	own    bool
}

func (s livenessStub) Descriptor() agent.AdapterDescriptor {
	d := s.stubCursorAdapter.Descriptor()
	d.Kind, d.Label = "fixture", "Fixture"
	return d
}

func (s livenessStub) Liveness(agent.LivenessView, agent.LivenessProbes) model.Liveness {
	return s.result
}

func (s livenessStub) TracksOwnProcess() bool { return s.own }

func TestSessionLivenessTrustsOnlyAdapterEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		result model.Liveness
		want   model.Liveness
		listed bool
	}{
		{name: "alive", result: "alive", want: "alive", listed: true},
		{name: "unknown", result: "unknown", want: "unknown", listed: true},
		{name: "exited", result: "exited", want: "exited", listed: false},
		{name: "garbage", result: "shared-daemon", want: "unknown", listed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := agent.NewRegistry(livenessStub{result: test.result, own: true})
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
			if err != nil {
				t.Fatal(err)
			}
			session, key, err := store.Register("fixture", newID(), "/workspace", nil)
			if err != nil {
				t.Fatal(err)
			}
			session.Owner = &model.ProcessRef{PID: 42, Start: "gone", Role: "client"}
			if err := store.locked(func() error { return store.saveSession(session) }); err != nil {
				t.Fatal(err)
			}
			if got := store.sessionLiveness(session); got != test.want {
				t.Fatalf("liveness %s, want %s", got, test.want)
			}
			listed, err := store.List()
			if err != nil {
				t.Fatal(err)
			}
			if found := len(listed) == 1 && listed[0].Liveness == test.want; found != test.listed {
				t.Fatalf("listed=%v sessions=%+v", found, listed)
			}
			_, err = (&Service{Store: store}).Send(context.Background(), SendRequest{Actor: session.ID, Capability: key, To: session.ID, MessageID: newID(), Body: "ping"})
			if test.result == "exited" {
				if err == nil {
					t.Fatal("sent to a session with positive exit evidence")
				}
			}
		})
	}
}

func TestSharedProcessOwnerIsNotRecorded(t *testing.T) {
	registry, err := agent.NewRegistry(livenessStub{result: "unknown", own: false})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := store.Register("fixture", newID(), "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	session.Owner = &model.ProcessRef{PID: 7, Start: "helper", Role: "client"}
	if err := store.locked(func() error { return store.saveSession(session) }); err != nil {
		t.Fatal(err)
	}
	if err := store.rememberOwner(session.ID, session.Kind, true); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(session.ID)
	if err != nil || current.Owner != nil {
		t.Fatalf("shared helper stayed recorded: %+v %v", current.Owner, err)
	}
	report, err := store.Prune(false, nil)
	if err != nil || len(report.Sessions) != 0 {
		t.Fatalf("unknown liveness pruned a session: %+v %v", report, err)
	}
}
