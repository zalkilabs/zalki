// This public-surface test pins diagnostic cause access before Receipt moves.
// It intentionally remains in bus_test to verify the later type alias too.
package bus_test

import (
	"errors"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestReceiptCauseAccessor(t *testing.T) {
	original := model.Receipt{}
	if original.Cause() != nil {
		t.Fatal("zero receipt has a cause")
	}
	cause := errors.New("private diagnostic")
	withCause := original.WithCause(cause)
	if got := withCause.Cause(); got != cause { //nolint:errorlint // Cause must retain the exact error interface identity, not an unwrap-compatible replacement.
		t.Fatalf("Cause()=%v, want original error identity", got)
	}
	if original.Cause() != nil {
		t.Fatal("WithCause changed the original receipt")
	}
	cleared := withCause.WithCause(nil)
	if cleared.Cause() != nil {
		t.Fatal("WithCause(nil) did not clear the copied cause")
	}
	if got := withCause.Cause(); got != cause { //nolint:errorlint // Clearing a copy must not change the original error identity.
		t.Fatal("clearing a copy changed the source receipt")
	}
}
