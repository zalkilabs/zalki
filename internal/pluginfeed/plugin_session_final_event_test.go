package pluginfeed

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

type endedPolicyCursor struct{ stubCursorAdapter }

func (endedPolicyCursor) Liveness(agent.LivenessView, agent.LivenessProbes) model.Liveness {
	return model.LivenessExited
}
func (endedPolicyCursor) TracksOwnProcess() bool { return false }

func TestSessionControlRestrictedFinalEventsNeverEscape(t *testing.T) {
	for _, kind := range []string{"muted-retirement", "off-owner-exit"} {
		t.Run(kind, func(t *testing.T) { assertRestrictedFinalEvent(t, kind) })
	}
}

func assertRestrictedFinalEvent(t *testing.T, kind string) {
	t.Helper()
	f, _, _ := externalPinFixture(t)
	if err := f.GrantPluginAccess(PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	offer, err := f.NextPluginFeed(&status)
	if err != nil {
		t.Fatal(err)
	}
	if offer.ID != "" {
		if err := f.AckPluginFeed(offer); err != nil {
			t.Fatal(err)
		}
	}
	active, _ := register(t, f.store, "cursor")
	if err := f.store.Update(active.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	assertActiveFinalEventControl(t, f, &status, active.ID)
	if kind == "off-owner-exit" {
		t.Setenv("ZALKI_PLUGINS", "off")
	}
	current, key := register(t, f.store, "cursor")
	if kind == "muted-retirement" {
		if err := bus.SetSessionPlugin(f.store, current.ID, key, status.Name, true); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Update(current.ID, model.StateRetired, true); err != nil {
			t.Fatal(err)
		}
	} else {
		registry, err := agent.NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, endedPolicyCursor{})
		if err != nil {
			t.Fatal(err)
		}
		f.store.Registry = registry
		if _, err := f.store.Prune(false, nil); err != nil {
			t.Fatal(err)
		}
	}
	assertNoRestrictedFinalOffer(t, f, &status, current.ID, kind)
}

func assertNoRestrictedFinalOffer(t *testing.T, f PluginFeed, status *PluginFeedStatus, id, kind string) {
	t.Helper()
	for range 16 {
		offer, err := f.NextPluginFeed(status)
		if err != nil {
			t.Fatal(err)
		}
		if offer.ID == "" {
			return
		}
		if offer.Event != nil && offer.Event.Message == nil && offer.Event.Session.ID == id {
			t.Fatal("Z6_RESTRICTED_FINAL_EVENT_NO_RICH_ESCAPE", kind)
		}
		if err := f.AckPluginFeed(offer); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("Z6_FINAL_EVENT_CONTROL_REACHES_HEAD")
}

func assertActiveFinalEventControl(t *testing.T, f PluginFeed, status *PluginFeedStatus, id string) {
	t.Helper()
	seen := false
	for range 16 {
		offer, err := f.NextPluginFeed(status)
		if err != nil {
			t.Fatal(err)
		}
		if offer.ID == "" {
			if !seen {
				t.Fatal("Z6_FINAL_EVENT_ACTIVE_POSITIVE")
			}
			return
		}
		if offer.Event != nil && offer.Event.Message == nil && offer.Event.Session.ID == id {
			seen = true
		}
		if err := f.AckPluginFeed(offer); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("Z6_FINAL_EVENT_ACTIVE_CONTROL_REACHES_HEAD")
}
