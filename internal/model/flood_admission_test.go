package model

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func newFloodAdmissionState(now time.Time) FloodState {
	return NewFloodState(now)
}

func TestFloodAdmissionPureRetryRefillAndRollback(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	state := newFloodAdmissionState(now)
	for i := range floodPairBurst {
		changed, err := applyFloodAdmission(&state, "sender", fmt.Sprint(i), now)
		if err != nil || !changed {
			t.Fatal("burst admission failed", i, err)
		}
	}
	for _, at := range []time.Time{now, now.Add(-time.Hour), now.Add(5*time.Second - time.Nanosecond)} {
		changed, err := applyFloodAdmission(&state, "sender", "next", at)
		if changed || ErrorCodeOf(err) != CodeRateLimited || !strings.Contains(err.Error(), "pair short bucket") {
			t.Fatal("burst/rollback refusal changed", err)
		}
	}
	changed, err := applyFloodAdmission(&state, "sender", "0", now)
	if err != nil || changed || len(state.Recent) != floodPairBurst {
		t.Fatal("precommit retry spent quota or requested a file rewrite", err)
	}
	if _, err := applyFloodAdmission(&state, "other", "0", now); ErrorCodeOf(err) != CodeConflict {
		t.Fatal("another sender reused reservation", err)
	}
	changed, err = applyFloodAdmission(&state, "sender", "next", now.Add(5*time.Second))
	if err != nil || !changed {
		t.Fatal("exact refill boundary did not admit", err)
	}
}

func TestFloodAdmissionPureHourlyWindowBoundaries(t *testing.T) {
	for _, scope := range []string{"pair", "inbound"} {
		t.Run(scope, func(t *testing.T) {
			now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			state := newFloodAdmissionState(now)
			limit, spacing, senders := floodPairPerHour, 6*time.Second, 1
			if scope == "inbound" {
				limit, spacing, senders = floodInboundPerHour, 2*time.Second, 5
			}
			for i := range limit {
				if changed, err := applyFloodAdmission(&state, fmt.Sprint(i%senders), fmt.Sprint(i), now.Add(time.Duration(i)*spacing)); err != nil || !changed {
					t.Fatal(i, err)
				}
			}
			changed, err := applyFloodAdmission(&state, "0", "over", now.Add(time.Duration(limit)*spacing))
			if changed || ErrorCodeOf(err) != CodeRateLimited || !strings.Contains(err.Error(), scope+" hourly cap") {
				t.Fatal("hourly cap not enforced", err)
			}
			if changed, err := applyFloodAdmission(&state, "0", "after", now.Add(time.Hour)); err != nil || !changed {
				t.Fatal("rolling window did not expire at boundary", err)
			}
		})
	}
}

// applyFloodAdmission applies successful candidates only, as the host does.
func applyFloodAdmission(state *FloodState, sender, id string, now time.Time) (bool, error) {
	decision := DecideFloodAdmission(*state, sender, id, now)
	if decision.Code != "" {
		return false, NewError(decision.Code, "%s", decision.Reason)
	}
	if decision.Changed {
		*state = decision.Next
	}
	return decision.Changed, nil
}
