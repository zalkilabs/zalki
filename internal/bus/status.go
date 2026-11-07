package bus

import "github.com/zalkilabs/zalki/internal/model"

// DeliveryStatus is the compatibility name for model.DeliveryStatus.
type DeliveryStatus = model.DeliveryStatus

// SessionState is the compatibility name for model.SessionState.
type SessionState = model.SessionState

const (
	StateReady              = model.StateReady
	StateBusy               = model.StateBusy
	StateApproval           = model.StateApproval
	StateRetired            = model.StateRetired
	StatusAttempting        = model.StatusAttempting
	StatusSubmitted         = model.StatusSubmitted
	StatusTransportWritten  = model.StatusTransportWritten
	StatusPendingActivity   = model.StatusPendingActivity
	StatusWakeWritten       = model.StatusWakeWritten
	StatusWakeUncertain     = model.StatusWakeUncertain
	StatusContextEmitted    = model.StatusContextEmitted
	StatusNotSent           = model.StatusNotSent
	StatusApprovalBlocked   = model.StatusApprovalBlocked
	StatusQueuedForApproval = model.StatusQueuedForApproval
	StatusCommandFailed     = model.StatusCommandFailed
	StatusUnknownTimeout    = model.StatusUnknownTimeout
	StatusUnknownWrite      = model.StatusUnknownWrite
)

func hookPendingStatus(s DeliveryStatus) bool { return model.HookPendingStatus(s) }

// transition forwards the pure delivery rule to model.
func transition(from, to DeliveryStatus, held bool) (DeliveryStatus, error) {
	return model.Transition(from, to, held)
}

const approvalQueuedNote = model.ApprovalQueuedNote

// ErrorCode is the compatibility name for model.ErrorCode.
type ErrorCode = model.ErrorCode

const (
	CodeInvalidInput      = model.CodeInvalidInput
	CodeInvalidIdentity   = model.CodeInvalidIdentity
	CodeUnauthorized      = model.CodeUnauthorized
	CodeConflict          = model.CodeConflict
	CodeUnavailable       = model.CodeUnavailable
	CodeRateLimited       = model.CodeRateLimited
	CodeNotFound          = model.CodeNotFound
	CodeUnsupportedSchema = model.CodeUnsupportedSchema
	CodeInvalidTransition = model.CodeInvalidTransition
	CodeInternal          = model.CodeInternal
)

// Error is the compatibility name for model.Error.
type Error = model.Error

func fail(code ErrorCode, format string, args ...any) error {
	return model.NewError(code, format, args...)
}

// ErrorCodeOf forwards error classification to model without changing state.
func ErrorCodeOf(err error) ErrorCode { return model.ErrorCodeOf(err) }
