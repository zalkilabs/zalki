package model

import (
	"errors"
	"fmt"
	"os"
)

// ErrorCode is the stable classification of a domain error.
type ErrorCode string

const (
	// CodeInvalidInput identifies invalid operation input.
	CodeInvalidInput ErrorCode = "invalid_input"
	// CodeInvalidIdentity identifies an invalid session or message identity.
	CodeInvalidIdentity ErrorCode = "invalid_identity"
	// CodeUnauthorized identifies an authentication or permission refusal.
	CodeUnauthorized ErrorCode = "unauthorized"
	// CodeConflict identifies conflicting state or ownership.
	CodeConflict ErrorCode = "conflict"
	// CodeUnavailable identifies an unavailable destination or delivery.
	CodeUnavailable ErrorCode = "unavailable"
	// CodeRateLimited identifies an exhausted delivery quota.
	CodeRateLimited ErrorCode = "rate_limited"
	// CodeNotFound identifies an absent resource.
	CodeNotFound ErrorCode = "not_found"
	// CodeUnsupportedSchema identifies an unsupported journal format.
	CodeUnsupportedSchema ErrorCode = "unsupported_schema"
	// CodeInvalidTransition identifies a disallowed state transition.
	CodeInvalidTransition ErrorCode = "invalid_transition"
	// CodeInternal identifies an otherwise unclassified failure.
	CodeInternal ErrorCode = "internal"
)

// Error carries a stable domain code and its public message.
type Error struct {
	// Code classifies the failure.
	Code ErrorCode
	// Message describes the failure without the code prefix.
	Message string
}

// Error returns the code-prefixed public message without changing the error.
func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// NewError constructs a coded error with fmt.Sprintf message formatting.
// It constructs a value; it does not apply a state transition or wrap a cause.
func NewError(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCodeOf inspects an error without changing it or application state.
// A coded error takes precedence over OS not-found and permission failures;
// nil has no code and all other errors are classified as internal.
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
