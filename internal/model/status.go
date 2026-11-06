package model

// DeliveryStatus is a persisted transport outcome or a derived response view.
type DeliveryStatus string

// SessionState is the recorded lifecycle state of a session.
type SessionState string

const (
	// StateReady identifies a session ready for work.
	StateReady SessionState = "ready"
	// StateBusy identifies a session working on a turn.
	StateBusy SessionState = "busy"
	// StateApproval identifies a session waiting for a permission prompt.
	StateApproval SessionState = "approval"
	// StateRetired identifies a retired session.
	StateRetired SessionState = "retired"
)

// Valid reports whether s is a supported persisted session state.
// It inspects the value and does not change session state.
func (s SessionState) Valid() bool {
	return s == StateReady || s == StateBusy || s == StateApproval || s == StateRetired
}

const (
	// StatusAttempting identifies an unsettled delivery attempt.
	StatusAttempting DeliveryStatus = "attempting"
	// StatusSubmitted identifies a transport-reported submission.
	StatusSubmitted DeliveryStatus = "submitted"
	// StatusTransportWritten identifies a completed transport write, not an acknowledgement.
	StatusTransportWritten DeliveryStatus = "transport_written"
	// StatusPendingActivity identifies delivery waiting for the next hook activity.
	StatusPendingActivity DeliveryStatus = "pending_next_activity"
	// StatusWakeWritten identifies a written wake hint.
	StatusWakeWritten DeliveryStatus = "wake_hint_written"
	// StatusWakeUncertain identifies an unconfirmed wake hint.
	StatusWakeUncertain DeliveryStatus = "wake_hint_uncertain"
	// StatusContextEmitted identifies emitted hook context.
	StatusContextEmitted DeliveryStatus = "hook_context_emitted"
	// StatusNotSent identifies a final refusal before submission.
	StatusNotSent DeliveryStatus = "not_sent"
	// StatusApprovalBlocked identifies an approval refusal, which may be held or final.
	StatusApprovalBlocked DeliveryStatus = "not_sent_pending_approval"
	// StatusQueuedForApproval is a derived response value, never a valid persisted status.
	StatusQueuedForApproval DeliveryStatus = "queued_for_approval"
	// StatusCommandFailed identifies a failed delivery command.
	StatusCommandFailed DeliveryStatus = "command_failed"
	// StatusUnknownTimeout identifies an uncertain submission after a timeout.
	StatusUnknownTimeout DeliveryStatus = "unknown_after_timeout"
	// StatusUnknownWrite identifies an uncertain submission after a write.
	StatusUnknownWrite DeliveryStatus = "unknown_after_write"
)

// Valid reports whether s is a supported persisted delivery status.
// It inspects the value and excludes the response-only queued approval view.
// It does not change a message or its persisted state.
func (s DeliveryStatus) Valid() bool {
	switch s {
	case StatusAttempting, StatusSubmitted, StatusTransportWritten, StatusPendingActivity,
		StatusWakeWritten, StatusWakeUncertain, StatusContextEmitted, StatusNotSent,
		StatusApprovalBlocked, StatusCommandFailed, StatusUnknownTimeout, StatusUnknownWrite:
		return true
	case StatusQueuedForApproval:
		return false
	}
	return false
}

// HookPendingStatus reports whether s is waiting for hook context delivery.
// It inspects the value; it does not emit context or change a message.
func HookPendingStatus(s DeliveryStatus) bool {
	return s == StatusPendingActivity || s == StatusWakeWritten || s == StatusWakeUncertain
}

// CanAcknowledge reports whether this delivery status permits acknowledgement.
// It inspects the status; it does not acknowledge or change a message.
func (s DeliveryStatus) CanAcknowledge() bool {
	return s.Valid() && s != StatusNotSent && s != StatusApprovalBlocked && s != StatusCommandFailed
}
