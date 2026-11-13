package pluginfeed

import (
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type sessionLockCursor struct {
	stubCursorAdapter
	probe func()
}

func (a sessionLockCursor) Liveness(agent.LivenessView, agent.LivenessProbes) model.Liveness {
	a.probe()
	return model.LivenessUnknown
}
func (sessionLockCursor) TracksOwnProcess() bool { return false }

func sessionLockStore(t *testing.T, s bus.Store, assertion string) (bus.Store, *int) {
	t.Helper()
	calls := 0
	probe := func() {
		calls++
		fd, err := journal.OpenLockFile(filepath.Join(s.Dir, "lock"), false)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = journal.CloseLockFile(fd) }()
		if err := journal.LockExclusive(fd, true); err == nil {
			_ = journal.Unlock(fd)
			t.Fatal(assertion)
		}
	}
	registry, err := agent.NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, sessionLockCursor{probe: probe})
	if err != nil {
		t.Fatal(err)
	}
	s.Registry = registry
	return s, &calls
}

func TestSessionControlEveryBoundaryHoldsLock(t *testing.T) {
	for _, kind := range []string{"A_feed", "B_inbox", "C_reply", "D_read", "E_owner_read"} {
		t.Run(kind, func(t *testing.T) { assertSessionBoundaryLock(t, kind) })
	}
}

func assertSessionBoundaryLock(t *testing.T, kind string) {
	t.Helper()
	assertion := "Z6_" + kind + "_LOCK_HELD"
	if kind == "D_read" || kind == "E_owner_read" {
		assertSessionReadBoundaryLock(t, kind, assertion)
		return
	}
	assertSessionMessageBoundaryLock(t, kind, assertion)
}
func assertSessionReadBoundaryLock(t *testing.T, kind, assertion string) {
	t.Helper()

	x := newPluginReadFixture(t, "cursor")
	store, calls := sessionLockStore(t, x.f.store, assertion)
	x.f.store = store
	if kind == "D_read" {
		if _, code, err := x.f.ReadPluginMessageBound(&x.before, x.target.ID, x.message.ID); err != nil || code != "ok" {
			t.Fatal("read positive", code, err)
		}
	} else {
		if _, err := x.f.ReadPluginMessage(x.target.ID, x.message.ID); err != nil {
			t.Fatal("owner read positive", err)
		}
	}
	if *calls == 0 {
		t.Fatal("Z6_LOCK_CONTROL_EXERCISED", kind)
	}
}
func assertSessionMessageBoundaryLock(t *testing.T, kind, assertion string) {
	t.Helper()

	x := newReplyFixture(t)
	store, calls := sessionLockStore(t, x.f.store, assertion)
	x.f.store = store
	x.service.Store = store
	x.sender.Store = store
	switch kind {
	case "A_feed":
		offer, err := x.f.NextPluginFeed(&x.status)
		if err != nil {
			t.Fatal(err)
		}
		if offer.ID != "" {
			if err := x.f.AckPluginFeed(offer); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Update(x.from.ID, model.StateBusy, false); err != nil {
			t.Fatal(err)
		}
		*calls = 0
		if _, err := x.f.NextPluginFeed(&x.status); err != nil {
			t.Fatal(err)
		}
	case "B_inbox":
		if _, err := x.service.Send(t.Context(), x.request()); err != nil {
			t.Fatal(err)
		}
	case "C_reply":
		if _, err := x.reply("lock-check", x.message.ID, x.from.ID); err != nil {
			t.Fatal(err)
		}
	}
	if *calls == 0 {
		t.Fatal("Z6_LOCK_CONTROL_EXERCISED", kind)
	}
}
