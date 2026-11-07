package model

import "time"

// ApprovalHoldDecision describes reconciliation of recorded and queued counts.
// It is a value-only result; the host reports and applies it under its lock.
type ApprovalHoldDecision struct {
	// Reason is the existing report reason, or empty when no action is needed.
	Reason string
	// SettleQueue requests settlement before any count is cleared.
	SettleQueue bool
	// ClearCount requests reloading and clearing the target's recorded count.
	ClearCount bool
}

// DecideApprovalHold compares recorded and queued counts using retirement facts.
// It neither mutates a session nor reads messages or writes counts.
func DecideApprovalHold(held, queued int, retired bool) ApprovalHoldDecision {
	switch {
	case retired && queued == 0 && held == 0:
		return ApprovalHoldDecision{}
	case retired && queued > 0:
		return ApprovalHoldDecision{Reason: "approval_settled", SettleQueue: true, ClearCount: true}
	case !retired && held == queued:
		return ApprovalHoldDecision{}
	default:
		return ApprovalHoldDecision{Reason: "approval_recounted", ClearCount: retired}
	}
}

// PruneRetirementReason decides whether supplied evidence calls for retirement.
// Explicit selection wins; unknown or live owner evidence does not retire.
// The host avoids the liveness probe entirely when selection is explicit.
func PruneRetirementReason(selected bool, liveness Liveness) string {
	switch {
	case selected:
		return "explicit_cleanup"
	case liveness == LivenessExited:
		return "owner_exited"
	default:
		return ""
	}
}

// RetiredLongEnough compares a timestamp with the supplied clock and window.
// Only strictly older instants qualify; an unparsable timestamp is retained.
func RetiredLongEnough(updatedAt string, now time.Time, window time.Duration) bool {
	updated, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return false
	}
	return updated.Before(now.Add(-window))
}
