package bus

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// A listener lease is bound to the generation of the registration it was taken
// for. The lock and the endpoint can both be fine and the listener still does
// not serve a newer generation. The status is read through the hook health
// interface, which does not depend on the type that holds the check.
func TestListenerRunsOnlyForTheGenerationOfItsLease(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	armedLease(t, store, to.ID, key)
	current, err := store.session(to.ID)
	if err != nil || current.Listener == nil || current.Listener.Generation != current.Generation {
		t.Fatalf("the armed lease must be bound to the generation of the session: %+v %v", current, err)
	}
	var health agent.HealthHost = hookRuntime{store: store}

	if got := health.Listener(current); got != model.ListenerRunning {
		t.Fatalf("an armed lease of the current generation reads %q, want running", got)
	}
	newer := current
	newer.Generation = newID()
	if got := health.Listener(newer); got != model.ListenerStale {
		t.Fatalf("a lease of an older generation reads %q, want stale", got)
	}
}
