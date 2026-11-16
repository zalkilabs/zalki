package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// pendingRecord is the record seam: unreadable, or this snapshot.
type pendingRecord struct {
	unreadable bool
	snap       pluginfeed.PluginFeedStatus
}

// pendingFixture is a started "demo" host whose record the test controls.
func pendingFixture(t *testing.T) (*supervisor, *supervised, *pendingRecord) {
	t.Helper()
	s, p := startedFixture(t)
	t.Cleanup(func() { s.stopAll("supervisor_stopped") })
	key := p.binding
	record := &pendingRecord{snap: pluginfeed.PluginFeedStatus{ID: key.id, Epoch: key.epoch, Revision: key.revision, Enabled: true}}
	s.snapshot = func(string) (pluginfeed.PluginFeedStatus, error) {
		if record.unreadable {
			return pluginfeed.PluginFeedStatus{}, errors.New("record unreadable")
		}
		return record.snap, nil
	}
	return s, p, record
}

// ZAL-74: a host that ends while its record is unreadable is neither a
// crash nor a stop; the first readable snapshot classifies it, once.
func TestPluginSupervisorUnknownSnapshotAtExitIsNotACrash(t *testing.T) {
	t.Run("unreadable", checkPendingStaysUnknown)
	t.Run("then disabled", func(t *testing.T) {
		_, p := endWhileUnreadable(t, func(r *pendingRecord) { r.snap.Enabled = false })
		if p.run.State != runStopped || p.run.ReasonCode != "disabled" || len(p.run.Restarts) != 0 || p.pendingEnd != "" {
			t.Fatalf("a pending end of a disabled plugin was not a stop: %q %q %d", p.run.State, p.run.ReasonCode, len(p.run.Restarts))
		}
	})
	t.Run("then enabled", checkPendingCountedOnce)
	t.Run("then waiting", checkPendingWaitingReasons)
	t.Run("waiting then stop", checkPendingWaitingStops)
	t.Run("then binding changed", func(t *testing.T) {
		s, p := endWhileUnreadable(t, func(r *pendingRecord) { r.snap.Revision = "revision-2" })
		if len(p.run.Restarts) != 0 || !supervisorLogHas(t, s, "demo: host ended: binding_changed") {
			t.Fatalf("a pending end of a changed binding was not a stop: %d restarts", len(p.run.Restarts))
		}
	})
	t.Run("readable at the end", func(t *testing.T) {
		s, p, _ := pendingFixture(t)
		s.finish("demo", p)
		if len(p.run.Restarts) != 1 || p.pendingEnd != "" {
			t.Fatalf("an end with a readable record is no longer one crash: %d", len(p.run.Restarts))
		}
	})
}

func checkPendingStaysUnknown(t *testing.T) {
	s, p, record := pendingFixture(t)
	before, err := os.ReadFile(runStatePath(s.state, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	record.unreadable = true
	s.finish("demo", p)
	for range 3 {
		s.reconcile("demo")
	}
	after, err := os.ReadFile(runStatePath(s.state, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.run.Restarts) != 0 || p.pendingEnd == "" || p.run.State == runBackoff {
		t.Fatalf("an unreadable record at exit was counted as a crash: %d restarts, pending %q", len(p.run.Restarts), p.pendingEnd)
	}
	if p.host != nil || !bytes.Equal(before, after) {
		t.Fatal("a pending end started a host or wrote the run file")
	}
}

func checkPendingCountedOnce(t *testing.T) {
	_, p := endWhileUnreadable(t, func(*pendingRecord) {})
	if len(p.run.Restarts) != 1 || p.run.State != runBackoff || p.run.ReasonCode != "failed" {
		t.Fatalf("an exit of an enabled plugin was not counted once the record was readable: %d %q %q", len(p.run.Restarts), p.run.State, p.run.ReasonCode)
	}
}

// endWhileUnreadable ends the host while the record is unreadable, then
// makes it readable (after change) and runs two polls.
func endWhileUnreadable(t *testing.T, change func(*pendingRecord)) (*supervisor, *supervised) {
	t.Helper()
	s, p, record := pendingFixture(t)
	record.unreadable = true
	s.finish("demo", p)
	record.unreadable = false
	change(record)
	s.reconcile("demo")
	counted := len(p.run.Restarts)
	s.reconcile("demo")
	if len(p.run.Restarts) != counted {
		t.Fatal("a pending end was counted twice")
	}
	return s, p
}

func supervisorLogHas(t *testing.T, s *supervisor, text string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.state, "plugins", ".supervisor.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), text)
}

// Pending waiting reasons keep the inherited R7 backoff without consuming a crash.
func checkPendingWaitingReasons(t *testing.T) {
	t.Helper()
	for _, reason := range []string{"lease_held", "log_failed"} {
		t.Run(reason, func(t *testing.T) {
			s, p, record := pendingFixture(t)
			p.lastReason = reason
			record.unreadable = true
			s.finish("demo", p)
			if p.pendingEnd != reason || len(p.run.Restarts) != 0 {
				t.Fatalf("an unreadable pending wait was classified: pending %q, restarts %d", p.pendingEnd, len(p.run.Restarts))
			}
			record.unreadable = false
			s.reconcile("demo")
			if p.run.State != runBackoff || p.run.ReasonCode != reason || len(p.run.Restarts) != 0 || p.pendingEnd != "" {
				t.Fatalf("a readable pending wait was not an uncounted wait: %q %q %d pending %q", p.run.State, p.run.ReasonCode, len(p.run.Restarts), p.pendingEnd)
			}
			if p.run.BackoffStep != 1 || !p.retryAt.After(s.now()) || p.host != nil {
				t.Fatalf("a pending wait restarted before its backoff: step %d, retry %s, host %v", p.run.BackoffStep, p.retryAt, p.host != nil)
			}
			waiting := p.retryAt
			s.reconcile("demo")
			if !p.retryAt.Equal(waiting) || len(p.run.Restarts) != 0 || p.host != nil {
				t.Fatal("a pending wait was classified twice or restarted before due")
			}
		})
	}
}

// A changed binding wins over a pending waiting reason, before any new start.
func checkPendingWaitingStops(t *testing.T) {
	t.Helper()
	t.Run("disabled", func(t *testing.T) {
		s, p, record := pendingFixture(t)
		p.lastReason = "lease_held"
		record.unreadable = true
		s.finish("demo", p)
		record.unreadable, record.snap.Enabled = false, false
		s.reconcile("demo")
		if p.run.State != runStopped || p.run.ReasonCode != "disabled" || len(p.run.Restarts) != 0 || p.pendingEnd != "" {
			t.Fatalf("disable did not win over a pending wait: %q %q %d pending %q", p.run.State, p.run.ReasonCode, len(p.run.Restarts), p.pendingEnd)
		}
	})
	t.Run("binding changed", func(t *testing.T) {
		s, p, record := pendingFixture(t)
		p.lastReason = "log_failed"
		record.unreadable = true
		s.finish("demo", p)
		record.unreadable = false
		record.snap.Revision = "revision-2"
		s.reconcile("demo")
		if len(p.run.Restarts) != 0 || p.pendingEnd != "" || !supervisorLogHas(t, s, "demo: host ended: binding_changed") {
			t.Fatalf("a binding change did not win over a pending wait: %d restarts, pending %q", len(p.run.Restarts), p.pendingEnd)
		}
	})
}
