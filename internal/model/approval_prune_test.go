// Compile-time signature checks restrict these rules to facts and values.
package model

import (
	"testing"
	"time"
)

var (
	_ func(int, int, bool) ApprovalHoldDecision   = DecideApprovalHold
	_ func(bool, Liveness) string                 = PruneRetirementReason
	_ func(string, time.Time, time.Duration) bool = RetiredLongEnough
)

func TestDecideApprovalHold(t *testing.T) {
	for _, test := range []struct {
		name         string
		held, queued int
		retired      bool
		want         ApprovalHoldDecision
	}{
		{"retired empty", 0, 0, true, ApprovalHoldDecision{}},
		{"retired stale count", 2, 0, true, ApprovalHoldDecision{Reason: "approval_recounted", ClearCount: true}},
		{"retired queue with dropped count", 0, 1, true, ApprovalHoldDecision{Reason: "approval_settled", SettleQueue: true, ClearCount: true}},
		{"retired inflated count", 5, 1, true, ApprovalHoldDecision{Reason: "approval_settled", SettleQueue: true, ClearCount: true}},
		{"retired matching queue still settles", 1, 1, true, ApprovalHoldDecision{Reason: "approval_settled", SettleQueue: true, ClearCount: true}},
		{"live empty", 0, 0, false, ApprovalHoldDecision{}},
		{"live matching", 1, 1, false, ApprovalHoldDecision{}},
		{"live dropped count", 0, 1, false, ApprovalHoldDecision{Reason: "approval_recounted"}},
		{"live stale count", 2, 0, false, ApprovalHoldDecision{Reason: "approval_recounted"}},
		{"live inflated count", 3, 1, false, ApprovalHoldDecision{Reason: "approval_recounted"}},
		{"retired negative recorded count", -1, 0, true, ApprovalHoldDecision{Reason: "approval_recounted", ClearCount: true}},
		{"live negative recorded count", -1, 0, false, ApprovalHoldDecision{Reason: "approval_recounted"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := DecideApprovalHold(test.held, test.queued, test.retired); got != test.want {
				t.Fatalf("decision=%+v, want %+v", got, test.want)
			}
		})
	}
}

func TestPruneRetirementReason(t *testing.T) {
	for _, test := range []struct {
		name     string
		selected bool
		liveness Liveness
		want     string
	}{
		{"selected unknown", true, "unknown", "explicit_cleanup"},
		{"selected exited", true, "exited", "explicit_cleanup"},
		{"selected alive", true, "alive", "explicit_cleanup"},
		{"exited", false, "exited", "owner_exited"},
		{"unknown", false, "unknown", ""},
		{"alive", false, "alive", ""},
		{"future evidence", false, "future_liveness", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := PruneRetirementReason(test.selected, test.liveness); got != test.want {
				t.Fatalf("reason=%q, want %q", got, test.want)
			}
		})
	}
}

func TestRetiredLongEnough(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, stamp string
		window      time.Duration
		want        bool
	}{
		{"exact boundary", "2026-10-25T12:00:00Z", 7 * 24 * time.Hour, false},
		{"one nanosecond older", "2026-10-25T11:59:59.999999999Z", 7 * 24 * time.Hour, true},
		{"future", "2026-11-02T12:00:00Z", 7 * 24 * time.Hour, false},
		{"unparseable", "not-a-time", 7 * 24 * time.Hour, false},
		{"zero window older", "2026-11-01T11:59:59.999999999Z", 0, true},
		{"zero window exact", "2026-11-01T12:00:00Z", 0, false},
		{"zero window future", "2026-11-02T12:00:00Z", 0, false},
		{"unparseable and zero window", "not-a-time", 0, false},
		{"same instant with offset", "2026-10-25T14:00:00+02:00", 7 * 24 * time.Hour, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := RetiredLongEnough(test.stamp, now, test.window); got != test.want {
				t.Fatalf("eligible=%v, want %v", got, test.want)
			}
		})
	}
}
