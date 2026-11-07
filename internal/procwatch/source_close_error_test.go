//go:build darwin || linux

package procwatch

import (
	"syscall"
	"testing"
)

func TestSourceClosePreservesBackendErrorAndIsIdempotent(t *testing.T) {
	source := closeErrorSource()
	if err := source.Close(); err != syscall.EBADF { //nolint:errorlint // Preserve the exact backend error value, not a wrapped replacement.
		t.Fatalf("first Close=%v, want exact EBADF", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("repeated Close=%v, want nil", err)
	}
	select {
	case _, ok := <-source.Events():
		if !ok {
			t.Fatal("Close closed the event channel")
		}
	default:
	}
}
