package bus

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// These tests pin four behaviors that mutation checks of the complexity
// refactors found untested: the sender label that EmitPending renders, the
// approval queue of a session whose record is unusable, the approval count of a
// retired session that has no queue, and quota files whose kind is not valid.
// They pin today's behavior; it is not judged here.

// A message written before labels were stored has no FromLabel. EmitPending
// renders it with the label of the sender's adapter, and does not save that
// label. Without it the envelope would say "Agent".
func TestEmitPendingRendersTheAdapterLabelOfARecordWithoutOne(t *testing.T) {
	f := newEmitFixture(t)
	adapter, err := f.store.registry().Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	label := adapter.Descriptor().Label
	if label == "" || label == "Agent" {
		t.Fatalf("the test needs a real adapter label, got %q", label)
	}
	msg := f.queue(t, "hello", time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	f.rewrite(t, msg.ID, func(m *model.Message) { m.FromLabel = "" })

	context, err := f.store.EmitPending(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(context, msg.ID) {
		t.Fatalf("the message was not emitted: %q", context)
	}
	if want := "From: " + label + " · "; !strings.Contains(context, want) {
		t.Fatalf("the envelope has no sender label %q: %q", label, context)
	}
	if strings.Contains(context, "From: Agent") {
		t.Fatalf("the envelope fell back to the generic label: %q", context)
	}
	if saved := f.read(t, msg.ID); saved.FromLabel != "" {
		t.Fatalf("the rendered label was saved: %+v", saved)
	}
}

// A session file that cannot be used (here: a schema from a newer build) is
// reported, but its record still exists. Messages queued for it are not the
// queue of a missing target, so prune must not settle them.
func TestPruneDoesNotSettleTheQueueOfASessionWhoseRecordIsUnusable(t *testing.T) {
	store := fixtureStore(t)
	sender, _ := register(t, store, "codex")
	broken := "cursor:00000000-0000-4000-8000-0000000000e0"
	if err := os.WriteFile(mustSessionPath(t, store, broken), []byte(`{"schema":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	queued := queuedFor(t, store, sender, broken)
	want := []PruneItem{{ID: broken, Reason: "session_unsupported_schema"}}

	for _, dryRun := range []bool{true, false} {
		report, err := store.Prune(dryRun, nil)
		if err != nil || !slices.Equal(report.Sessions, want) {
			t.Fatalf("dry=%v: reported %v (%v), want only %v", dryRun, report.Sessions, err, want)
		}
		if current := journalMessage(t, store, queued.ID); !current.Held || current.Status != model.StatusApprovalBlocked {
			t.Fatalf("dry=%v: the queue of a session with an unusable record was settled: %+v", dryRun, current)
		}
	}
}

// A retired session with an approval count but nothing queued is recounted to
// zero. A retired session with no count and no queue is left alone.
func TestPruneRecountsARetiredSessionThatHoldsACountButHasNoQueue(t *testing.T) {
	store := fixtureStore(t)
	held := retiredWithHeld(t, store, "01", 2)
	idle := retiredWithHeld(t, store, "02", 0)
	want := []PruneItem{{ID: held.ID, Reason: "approval_recounted"}}

	preview, err := store.Prune(true, nil)
	if err != nil || !slices.Equal(preview.Sessions, want) {
		t.Fatalf("dry-run reported %v (%v), want %v", preview.Sessions, err, want)
	}
	assertHoldCounts(t, store, map[string]int{held.ID: 2, idle.ID: 0}, true)

	report, err := store.Prune(false, nil)
	if err != nil || !slices.Equal(report.Sessions, want) {
		t.Fatalf("applied run reported %v (%v), want the dry-run list %v", report.Sessions, err, want)
	}
	assertHoldCounts(t, store, map[string]int{held.ID: 0, idle.ID: 0}, true)
	for _, id := range []string{held.ID, idle.ID} {
		if current, err := store.session(id); err != nil || !current.Retired {
			t.Fatalf("session %s after prune: retired=%v (%v), want it still retired", id, current.Retired, err)
		}
	}
	if again, err := store.Prune(false, nil); err != nil || len(again.Sessions) != 0 {
		t.Fatalf("a second run found more to do: %v %v", again.Sessions, err)
	}
}

// retiredWithHeld registers a cursor session, retires it, and stores the given
// approval count on it.
func retiredWithHeld(t *testing.T, store Store, last string, held int) model.Session {
	t.Helper()
	v := fixedSession(t, store, last)
	if err := store.Update(v.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	return withHeld(t, store, current, held)
}

// A quota file is <kind>_<canonical uuid>.json, and the kind must be a valid
// adapter kind name: lower case, starting with a letter. A file with another
// kind is not a quota record, even when its session is gone; it is left alone.
func TestPruneFloodKeepsAQuotaFileWhoseKindIsNotValid(t *testing.T) {
	store := fixtureStore(t)
	dir := filepath.Join(store.Dir, "flood")
	if err := store.locked(func() error { return privateDir(dir) }); err != nil {
		t.Fatal(err)
	}
	orphan := "cursor_" + newID() + ".json"
	invalid := []string{"Cursor_" + newID() + ".json", "9lives_" + newID() + ".json", "cur sor_" + newID() + ".json"}
	for _, base := range append([]string{orphan}, invalid...) {
		if err := os.WriteFile(filepath.Join(dir, base), []byte(`{"schema":1}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{orphan}

	for _, dryRun := range []bool{true, false} {
		report, err := store.Prune(dryRun, nil)
		if err != nil || !slices.Equal(report.DeletedFlood, want) {
			t.Fatalf("dry=%v: listed %v (%v), want only %v", dryRun, report.DeletedFlood, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, orphan)); !os.IsNotExist(err) {
		t.Fatalf("the valid orphan remained: %v", err)
	}
	for _, base := range invalid {
		if _, err := os.Lstat(filepath.Join(dir, base)); err != nil {
			t.Fatalf("a file with an invalid kind was removed: %s: %v", base, err)
		}
	}
}
