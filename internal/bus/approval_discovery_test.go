package bus

import (
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// holdTwo stores two messages held for target's permission prompt and returns
// the older one.
func holdTwo(t *testing.T, store Store, from, target model.Session) model.Message {
	t.Helper()
	var first model.Message
	for i := range 2 {
		msg := model.Message{ID: newID(), From: from.ID, To: target.ID, Body: "held", Status: model.StatusAttempting, TargetGeneration: target.Generation, CreatedAt: model.Timestamp()}
		if err := msg.Hold(); err != nil {
			t.Fatal(err)
		}
		if err := store.locked(func() error { return approvals(store).saveReservation(msg) }); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = msg
		}
		time.Sleep(2 * time.Millisecond)
	}
	return first
}

func agentRow(t *testing.T, rows []AgentInfo, id string) AgentInfo {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("no row for %s in %+v", id, rows)
	return AgentInfo{}
}

func requireSameFiles(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("discovery changed the stored files: %d before, %d after", len(before), len(after))
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("discovery changed the stored file %s", name)
		}
	}
}

// A session with held messages shows "approval" and since when in the discovery
// view, also when its own state is busy (Codex). The view is computed on read:
// no stored record, event or session state changes, so the plugin feed, which
// is built from the stored records and events, is unchanged.
func TestDiscoveryShowsApprovalSinceForHeldMessagesWithoutChangingStorage(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "codex")
	viewer, viewerKey := register(t, store, "cursor")
	if err := store.Update(target.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	first := holdTwo(t, store, viewer, target)
	before := reservationSnapshot(t, store.dir)
	rows, _, err := store.DiscoverWithWarnings(viewer.ID, viewerKey)
	if err != nil {
		t.Fatal(err)
	}
	held, other := agentRow(t, rows, target.ID), agentRow(t, rows, viewer.ID)
	if held.State != model.StateApproval || held.ApprovalSince != first.Receipt.At {
		t.Fatalf("held session view: state=%q since=%q, want approval since the oldest hold %q", held.State, held.ApprovalSince, first.Receipt.At)
	}
	if other.State != "" || other.ApprovalSince != "" {
		t.Fatalf("a session with no held message changed: %+v", other)
	}
	requireSameFiles(t, before, reservationSnapshot(t, store.dir))
	stored, err := store.session(target.ID)
	if err != nil || stored.State != model.StateBusy || stored.ApprovalHeld != 2 {
		t.Fatalf("stored session = state %q, held %d, %v; want busy and 2", stored.State, stored.ApprovalHeld, err)
	}
}

// The oldest hold is found by its instant, not by comparing the text of the time:
// "…21:00:00.1Z" is earlier than "…21:00:00.11Z", but its text sorts after it.
func TestOldestHoldComparesInstantsNotText(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "codex")
	viewer, _ := register(t, store, "cursor")
	for _, at := range []string{"2026-11-22T21:00:00.11Z", "2026-11-22T21:00:00.1Z", "2026-11-22T21:00:00.12Z"} {
		msg := model.Message{ID: newID(), From: viewer.ID, To: target.ID, Body: "held", Status: model.StatusAttempting, TargetGeneration: target.Generation, CreatedAt: model.Timestamp()}
		if err := msg.Hold(); err != nil {
			t.Fatal(err)
		}
		msg.Receipt.At = at
		if err := store.locked(func() error { return approvals(store).saveReservation(msg) }); err != nil {
			t.Fatal(err)
		}
	}
	var got string
	if err := store.readLocked(func() error { got = oldestHold(store, target.ID); return nil }); err != nil {
		t.Fatal(err)
	}
	if got != "2026-11-22T21:00:00.1Z" {
		t.Fatalf("oldest hold = %q, want 2026-11-22T21:00:00.1Z", got)
	}
}
