package bus

import (
	"bytes"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type exitedPluginCursor struct{ stubCursorAdapter }

func (exitedPluginCursor) Liveness(agent.LivenessView, agent.LivenessProbes) model.Liveness {
	return model.LivenessExited
}
func (exitedPluginCursor) TracksOwnProcess() bool { return false }

func TestSessionPluginAuthenticatedRetiredSelfIsRefused(t *testing.T) {
	s := fixtureStore(t)
	v, key := register(t, s, "cursor")
	if err := s.Update(v.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	path, _ := s.sessionPath(v.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetSessionPlugin(s, v.ID, key, "external", true); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatal("Z6_SELF_RETIRED_MUTE_REFUSED", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("Z6_RETIRED_REFUSAL_PRESERVES_RECORD", err)
	}
}

func TestSessionPluginAuthenticatedExitedSelfIsRefused(t *testing.T) {
	s := fixtureStore(t)
	var err error
	s.Registry, err = agent.NewRegistry(exitedPluginCursor{})
	if err != nil {
		t.Fatal(err)
	}
	v, key := register(t, s, "cursor")
	if err := SetSessionPlugin(s, v.ID, key, "external", true); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatal("Z6_SELF_EXITED_MUTE_REFUSED", err)
	}
	_, policy := pluginPolicyForTest(t, s, v.ID)
	if len(policy.Muted) != 0 {
		t.Fatal("Z6_EXITED_REFUSAL_PRESERVES_POLICY")
	}
}

func TestSessionPluginStaleCapabilityCannotUnmute(t *testing.T) {
	s := fixtureStore(t)
	v, key := register(t, s, "cursor")
	if err := SetSessionPlugin(s, v.ID, key, "external", true); err != nil {
		t.Fatal(err)
	}
	_, fresh, err := s.Register("cursor", v.NativeID, v.CWD, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetSessionPlugin(s, v.ID, key, "external", false); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("Z6_STALE_CAPABILITY_CANNOT_UNMUTE", err)
	}
	_, policy := pluginPolicyForTest(t, s, v.ID)
	if _, ok := policy.Muted["external"]; !ok {
		t.Fatal("Z6_STALE_KEY_CANNOT_CLEAR_OVERLAY")
	}
	if err := SetSessionPlugin(s, v.ID, fresh, "external", false); err != nil {
		t.Fatal(err)
	}
}
