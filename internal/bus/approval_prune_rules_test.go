// These cases guard probe suppression and errors that must precede IO.
package bus

import (
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
)

type pruneProbeAdapter struct {
	stubCursorAdapter
	calls *int
}

var _ agent.LivenessAdapter = pruneProbeAdapter{}

func (a pruneProbeAdapter) Liveness(agent.LivenessView, agent.LivenessProbes) Liveness {
	(*a.calls)++
	return LivenessExited
}

func (pruneProbeAdapter) TracksOwnProcess() bool { return false }

func TestExplicitPruneDoesNotProbeSessionLiveness(t *testing.T) {
	for _, dry := range []bool{true, false} {
		store := fixtureStore(t)
		calls := 0
		registry, err := NewRegistry(pruneProbeAdapter{calls: &calls})
		if err != nil {
			t.Fatal(err)
		}
		store.Registry = registry
		target, _ := register(t, store, "cursor")
		calls = 0
		if got := store.sessionLiveness(target); got != LivenessExited || calls != 1 {
			t.Fatal("fixture liveness policy is not wired to its counter")
		}
		calls = 0
		report, err := store.Prune(dry, []string{target.ID})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatalf("dry=%v: explicit selection called liveness policy %d times", dry, calls)
		}
		if len(report.Sessions) != 1 || report.Sessions[0].Reason != "explicit_cleanup" {
			t.Fatalf("dry=%v: explicit selection report=%+v", dry, report.Sessions)
		}
	}
}

func TestNegativeRetentionPrecedesInvalidSelection(t *testing.T) {
	store := fixtureStore(t)
	for _, dry := range []bool{true, false} {
		report, err := store.PruneRetired(dry, []string{"cursor:invalid-selection"}, -time.Nanosecond)
		if err == nil || err.Error() != "retired retention window must not be negative" {
			t.Fatalf("dry=%v: first error=%v, want negative retention", dry, err)
		}
		if len(report.Sessions) != 0 || report.DryRun != dry {
			t.Fatalf("invalid configuration changed report: %+v", report)
		}
	}
}
