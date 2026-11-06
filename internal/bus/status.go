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

// Attempts accept delivery receipts; pending states may emit hook context.
// Emitted context wins a racing wake receipt. Only a held approval may return
// to attempting/pending or settle not-sent; legacy refusals cannot reopen.
func transition(from, to DeliveryStatus, held bool) (DeliveryStatus, error) {
	if held && from == StatusApprovalBlocked && (to == StatusAttempting || to == StatusPendingActivity || to == StatusNotSent) {
		return to, nil
	}
	if !to.Valid() || to == StatusAttempting {
		return from, fail(CodeInvalidTransition, "invalid delivery receipt %q", to)
	}
	// Hook context may be emitted while a wake transport is completing.
	if from == StatusContextEmitted && (hookPendingStatus(to) || to == from) {
		return from, nil
	}
	if from == to || from == StatusAttempting || (hookPendingStatus(from) && (hookPendingStatus(to) || to == StatusContextEmitted)) {
		return to, nil
	}
	return from, fail(CodeInvalidTransition, "cannot change delivery status from %q to %q", from, to)
}

const approvalQueuedNote = "queued until the permission prompt is answered"

// IsQueuedForApproval distinguishes a held message from a legacy final refusal.
// The derived queued_for_approval view is never written to the journal.
func (msg Message) IsQueuedForApproval() bool {
	return msg.Held && msg.Status == StatusApprovalBlocked
}

func (msg *Message) move(to DeliveryStatus, held bool) error {
	next, err := transition(msg.Status, to, held)
	if err != nil {
		return err
	}
	msg.Status = next
	return nil
}

func (msg *Message) hold() error {
	if err := msg.move(StatusApprovalBlocked, false); err != nil {
		return err
	}
	msg.Held = true
	msg.Receipt = &Receipt{At: timestamp(), Status: StatusApprovalBlocked, Error: approvalQueuedNote}
	return nil
}

func (msg *Message) settleNotSent(code ErrorCode, reason string) error {
	if err := msg.move(StatusNotSent, msg.IsQueuedForApproval()); err != nil {
		return err
	}
	msg.Held = false
	msg.Receipt = &Receipt{At: timestamp(), Status: StatusNotSent, Code: code, Error: reason}
	return nil
}

func (msg *Message) releaseHeld(to DeliveryStatus) error {
	if !msg.IsQueuedForApproval() || (to != StatusAttempting && to != StatusPendingActivity) {
		return fail(CodeInvalidTransition, "only a held message may return to attempting or pending delivery")
	}
	if err := msg.move(to, true); err != nil {
		return err
	}
	msg.Held = false
	msg.Receipt = nil
	if to == StatusPendingActivity {
		msg.Receipt = &Receipt{At: timestamp(), Status: to}
	}
	return nil
}

func (msg *Message) emitContext() error {
	if err := msg.move(StatusContextEmitted, false); err != nil {
		return err
	}
	msg.Receipt = &Receipt{At: timestamp(), Status: StatusContextEmitted}
	return nil
}

func (msg *Message) applyReceipt(receipt Receipt) error {
	if err := msg.move(receipt.Status, false); err != nil {
		return err
	}
	if receipt.Error != "" && receipt.Code == "" {
		receipt.Code = CodeUnavailable
	}
	msg.Receipt = &receipt
	return nil
}

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
