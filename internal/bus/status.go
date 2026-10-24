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
	StatusCommandFailed    DeliveryStatus = "command_failed"
	StatusUnknownTimeout   DeliveryStatus = "unknown_after_timeout"
	StatusUnknownWrite     DeliveryStatus = "unknown_after_write"
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

func transition(from, to DeliveryStatus) (DeliveryStatus, error) {
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

type ErrorCode string

const (
	CodeInvalidInput      ErrorCode = "invalid_input"
	CodeInvalidIdentity   ErrorCode = "invalid_identity"
	CodeUnauthorized      ErrorCode = "unauthorized"
	CodeConflict          ErrorCode = "conflict"
	CodeUnavailable       ErrorCode = "unavailable"
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
