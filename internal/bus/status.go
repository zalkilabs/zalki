package bus

import (
	"errors"
	"fmt"
	"os"
)

type DeliveryStatus string

type SessionState string

const (
	StateReady    SessionState = "ready"
	StateBusy     SessionState = "busy"
	StateApproval SessionState = "approval"
	StateRetired  SessionState = "retired"
)

func (s SessionState) valid() bool {
	return s == StateReady || s == StateBusy || s == StateApproval || s == StateRetired
}

const (
	StatusAttempting       DeliveryStatus = "attempting"
	StatusSubmitted        DeliveryStatus = "submitted"
	StatusTransportWritten DeliveryStatus = "transport_written"
	StatusPendingActivity  DeliveryStatus = "pending_next_activity"
	StatusWakeWritten      DeliveryStatus = "wake_hint_written"
	StatusWakeUncertain    DeliveryStatus = "wake_hint_uncertain"
	StatusContextEmitted   DeliveryStatus = "hook_context_emitted"
	StatusNotSent          DeliveryStatus = "not_sent"
	StatusApprovalBlocked  DeliveryStatus = "not_sent_pending_approval"
	// StatusQueuedForApproval is a derived response value, never a valid persisted status.
	StatusQueuedForApproval DeliveryStatus = "queued_for_approval"
	StatusCommandFailed     DeliveryStatus = "command_failed"
	StatusUnknownTimeout    DeliveryStatus = "unknown_after_timeout"
	StatusUnknownWrite      DeliveryStatus = "unknown_after_write"
)

func (s DeliveryStatus) valid() bool {
	switch s {
	case StatusAttempting, StatusSubmitted, StatusTransportWritten, StatusPendingActivity,
		StatusWakeWritten, StatusWakeUncertain, StatusContextEmitted, StatusNotSent,
		StatusApprovalBlocked, StatusCommandFailed, StatusUnknownTimeout, StatusUnknownWrite:
		return true
	}
	return false
}

func hookPendingStatus(s DeliveryStatus) bool {
	return s == StatusPendingActivity || s == StatusWakeWritten || s == StatusWakeUncertain
}

func (s DeliveryStatus) canAcknowledge() bool {
	return s.valid() && s != StatusNotSent && s != StatusApprovalBlocked && s != StatusCommandFailed
}

// Attempts accept delivery receipts; pending states may emit hook context.
// Emitted context wins a racing wake receipt. Only a held approval may return
// to attempting/pending or settle not-sent; legacy refusals cannot reopen.
func transition(from, to DeliveryStatus, held bool) (DeliveryStatus, error) {
	if held && from == StatusApprovalBlocked && (to == StatusAttempting || to == StatusPendingActivity || to == StatusNotSent) {
		return to, nil
	}
	if !to.valid() || to == StatusAttempting {
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

type ErrorCode string

const (
	CodeInvalidInput      ErrorCode = "invalid_input"
	CodeInvalidIdentity   ErrorCode = "invalid_identity"
	CodeUnauthorized      ErrorCode = "unauthorized"
	CodeConflict          ErrorCode = "conflict"
	CodeUnavailable       ErrorCode = "unavailable"
	CodeRateLimited       ErrorCode = "rate_limited"
	CodeNotFound          ErrorCode = "not_found"
	CodeUnsupportedSchema ErrorCode = "unsupported_schema"
	CodeInvalidTransition ErrorCode = "invalid_transition"
	CodeInternal          ErrorCode = "internal"
)

type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }
func fail(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
func ErrorCodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var coded *Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	if errors.Is(err, os.ErrNotExist) {
		return CodeNotFound
	}
	if errors.Is(err, os.ErrPermission) {
		return CodeUnauthorized
	}
	return CodeInternal
}
