package journal_test

import (
	"syscall"
	"testing"
)

func requireErrno(t *testing.T, err error, want syscall.Errno) {
	t.Helper()
	got, ok := err.(syscall.Errno) //nolint:errorlint // The primitive must return the direct errno, not a wrapper that merely unwraps to it.
	if !ok || got != want {
		t.Fatalf("error = %T %v, want raw errno %d", err, err, want)
	}
}
