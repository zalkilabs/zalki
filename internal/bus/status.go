package bus

import "github.com/zalkilabs/zalki/internal/model"

// DeliveryStatus is the compatibility name for model.DeliveryStatus.
type DeliveryStatus = model.DeliveryStatus

// SessionState is the compatibility name for model.SessionState.
type SessionState = model.SessionState

// ErrorCode is the compatibility name for model.ErrorCode.
type ErrorCode = model.ErrorCode

// Error is the compatibility name for model.Error.
type Error = model.Error

// ErrorCodeOf forwards error classification to model without changing state.
func ErrorCodeOf(err error) ErrorCode { return model.ErrorCodeOf(err) }
