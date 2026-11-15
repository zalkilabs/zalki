package bus

import (
	"path/filepath"
	"testing"
)

func TestRuntimeActorUpdateSurvivesSchemaGuard(t *testing.T) {
	s := fixtureStore(t)
	actor, _ := register(t, s, "cursor")
	lease, err := s.TrackRuntime("mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	lease.ObserveActor(actor.ID)
	if info := lease.Info(); len(info.Actors) != 1 || info.Actors[0] != actor.ID {
		t.Fatal("RUNTIME_ACTOR_UPDATE_MISSING", info.Actors)
	}
	var saved RuntimeInfo
	if err := readJSON(filepath.Join(s.Dir, "runtimes", lease.Info().ID+".json"), &saved); err != nil || len(saved.Actors) != 1 || saved.Actors[0] != actor.ID {
		t.Fatal("RUNTIME_ACTOR_UPDATE_NOT_PERSISTED", err, saved.Actors)
	}
}
