package bus

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func selfProcess(t *testing.T) processInfo {
	t.Helper()
	p, err := readProcess(os.Getpid())
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("sandbox forbids process inspection: %v", err)
	}
	if err != nil || p.Start == "" || p.UID != uint32(os.Getuid()) {
		t.Fatalf("cannot identify current OS process: %+v %v", p, err)
	}
	return p
}

func TestProcessProbeDistinguishesExitReuseAndUnknown(t *testing.T) {
	ref := &ProcessRef{PID: 42, Start: "original", Role: "client"}
	for _, test := range []struct {
		name string
		p    processInfo
		err  error
		want string
	}{
		{"alive", processInfo{Start: "original", UID: uint32(os.Getuid())}, nil, "alive"},
		{"PID reused", processInfo{Start: "new", UID: uint32(os.Getuid())}, nil, "exited"},
		{"zombie", processInfo{Start: "original", UID: uint32(os.Getuid()), Zombie: true}, nil, "exited"},
		{"gone", processInfo{}, syscall.ESRCH, "exited"},
		{"permission denied", processInfo{}, syscall.EPERM, "unknown"},
		{"inspection failed", processInfo{}, errors.New("unavailable"), "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := probeProcess(ref, func(int) (processInfo, error) { return test.p, test.err }); got != test.want {
				t.Fatalf("got %s want %s", got, test.want)
			}
		})
	}
	if got := processLiveness(nil); got != "unknown" {
		t.Fatal("missing owner treated as exit")
	}
}

func TestOwnerCaptureUsesNativeAncestorAndLabelsBackend(t *testing.T) {
	for _, test := range []struct{ kind, name, role string }{{"cursor", "agent", "client"}, {"claude", "claude", "client"}, {"codex", "codex", "backend"}} {
		t.Run(test.kind, func(t *testing.T) {
			read := func(pid int) (processInfo, error) {
				name, parent := "sh", 99
				if pid == 99 {
					name, parent = test.name, 1
				}
				return processInfo{PID: pid, PPID: parent, Name: name, Start: "start", UID: uint32(os.Getuid())}, nil
			}
			adapter, err := testRegistry().Lookup(test.kind)
			if err != nil {
				t.Fatal(err)
			}
			ref := ownerFrom(adapter.Descriptor(), 100, read)
			if ref == nil || ref.PID != 99 || ref.Role != test.role {
				t.Fatalf("wrong native owner: %+v", ref)
			}
		})
	}
	if ref := ownerFrom((stubCursorAdapter{}).Descriptor(), 100, func(int) (processInfo, error) { return processInfo{}, syscall.EPERM }); ref != nil {
		t.Fatal("invented an owner without OS evidence")
	}
}

func TestProcessFingerprintTracksRealExit(t *testing.T) {
	selfProcess(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	p, err := readProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ref := &ProcessRef{PID: p.PID, Start: p.Start, Role: "client"}
	if processLiveness(ref) != "alive" {
		t.Fatal("live child not identified")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if processLiveness(ref) != "exited" {
		t.Fatal("exited child remained alive")
	}
}
