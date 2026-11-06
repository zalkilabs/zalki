//go:build darwin || linux

package procwatch

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestProcessGoneRequiresExitEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		gone bool
	}{
		{"no error", nil, false},
		{"not found", os.ErrNotExist, true},
		{"no process", syscall.ESRCH, true},
		{"wrapped not found", fmt.Errorf("inspect: %w", os.ErrNotExist), true},
		{"wrapped no process", fmt.Errorf("inspect: %w", syscall.ESRCH), true},
		{"permission denied", syscall.EPERM, false},
		{"inspection failed", syscall.EIO, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := processGone(test.err); got != test.gone {
				t.Fatalf("processGone(%v) = %v, want %v", test.err, got, test.gone)
			}
		})
	}
}

func TestReadAndNamedPreserveProcessIdentity(t *testing.T) {
	self, err := Read(os.Getpid())
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("sandbox forbids process inspection: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if self.PID != os.Getpid() || self.PPID != os.Getppid() || self.Start == "" || self.Name == "" || self.UID != uint32(os.Getuid()) || self.Zombie {
		t.Fatalf("invalid current process: %+v", self)
	}
	for _, test := range []struct {
		name string
		self bool
	}{
		{self.Name, true},
		{"zalki-process-name-that-cannot-be-the-test-binary", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows, err := Named(test.name)
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				t.Skipf("sandbox forbids process enumeration: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if row.Name != test.name || row.UID != uint32(os.Getuid()) || row.Zombie {
					t.Fatalf("Named returned a nonmatching process: %+v", row)
				}
				if row.PID == self.PID && row.Start == self.Start {
					found = true
				}
			}
			if found != test.self {
				t.Fatalf("current process found = %v, want %v", found, test.self)
			}
		})
	}
	if _, err := Read(1 << 30); !processGone(err) {
		t.Fatalf("missing process did not return exit evidence: %v", err)
	}
}
