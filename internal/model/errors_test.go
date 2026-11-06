// These in-package tests pin coded error construction and classification.
// Expected text, wire codes and priority are independent of implementation data.
package model

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestErrorCodeWireSpellings(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
	}{
		{"CodeInvalidInput", string(CodeInvalidInput), "invalid_input"},
		{"CodeInvalidIdentity", string(CodeInvalidIdentity), "invalid_identity"},
		{"CodeUnauthorized", string(CodeUnauthorized), "unauthorized"},
		{"CodeConflict", string(CodeConflict), "conflict"},
		{"CodeUnavailable", string(CodeUnavailable), "unavailable"},
		{"CodeRateLimited", string(CodeRateLimited), "rate_limited"},
		{"CodeNotFound", string(CodeNotFound), "not_found"},
		{"CodeUnsupportedSchema", string(CodeUnsupportedSchema), "unsupported_schema"},
		{"CodeInvalidTransition", string(CodeInvalidTransition), "invalid_transition"},
		{"CodeInternal", string(CodeInternal), "internal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.value != test.want {
				t.Fatalf("%s=%q, want %q", test.name, test.value, test.want)
			}
		})
	}
}

func TestErrorTextAndConstruction(t *testing.T) {
	if got := (&Error{Code: "conflict", Message: "already held"}).Error(); got != "conflict: already held" {
		t.Fatalf("Error()=%q", got)
	}
	err := NewError("invalid_input", "bad value %d for %q", 7, "name")
	if err == nil {
		t.Fatal("NewError returned nil")
	}
	if err.Error() != `invalid_input: bad value 7 for "name"` {
		t.Fatalf("NewError()=%v", err)
	}
	direct, ok := err.(*Error) //nolint:errorlint // The constructor promises a direct *Error, not merely an unwrap-compatible error.
	if !ok || direct.Code != "invalid_input" || direct.Message != `bad value 7 for "name"` {
		t.Fatalf("NewError()=%T %v, want direct *Error with literal code/message", err, err)
	}
	var recovered *Error
	if !errors.As(fmt.Errorf("wrapped: %w", err), &recovered) || recovered != direct {
		t.Fatal("wrapped NewError result lost its typed pointer identity")
	}
}

func TestErrorClassificationPriority(t *testing.T) {
	coded := &Error{Code: "conflict", Message: "refusal"}
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain unknown", errors.New("plain"), "internal"},
		{"typed", coded, "conflict"},
		{"wrapped typed", fmt.Errorf("outer: %w", coded), "conflict"},
		{"not found", os.ErrNotExist, "not_found"},
		{"wrapped not found", fmt.Errorf("outer: %w", os.ErrNotExist), "not_found"},
		{"permission", os.ErrPermission, "unauthorized"},
		{"wrapped permission", fmt.Errorf("outer: %w", os.ErrPermission), "unauthorized"},
		{"coded wins joined OS", errors.Join(os.ErrNotExist, coded), "conflict"},
		{"not found wins joined permission", errors.Join(os.ErrPermission, os.ErrNotExist), "not_found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := string(ErrorCodeOf(test.err)); got != test.want {
				t.Fatalf("ErrorCodeOf(%v)=%q, want %q", test.err, got, test.want)
			}
		})
	}
}
