package bus

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

func selfProcess(t *testing.T) procwatch.Info {
	t.Helper()
	p, err := procwatch.Read(os.Getpid())
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("sandbox forbids process inspection: %v", err)
	}
	if err != nil || p.Start == "" || p.UID != uint32(os.Getuid()) {
		t.Fatalf("cannot identify current OS process: %+v %v", p, err)
	}
	return p
}

func TestProcessProbeDistinguishesExitReuseAndUnknown(t *testing.T) {
	ref := &model.ProcessRef{PID: 42, Start: "original", Role: "client"}
	for _, test := range []struct {
		name string
		p    procwatch.Info
		err  error
		want model.Liveness
	}{
		{"alive", procwatch.Info{Start: "original", UID: uint32(os.Getuid())}, nil, "alive"},
		{"PID reused", procwatch.Info{Start: "new", UID: uint32(os.Getuid())}, nil, "exited"},
		{"zombie", procwatch.Info{Start: "original", UID: uint32(os.Getuid()), Zombie: true}, nil, "exited"},
		{"gone", procwatch.Info{}, syscall.ESRCH, "exited"},
		{"permission denied", procwatch.Info{}, syscall.EPERM, "unknown"},
		{"inspection failed", procwatch.Info{}, errors.New("unavailable"), "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := probeProcess(ref, func(int) (procwatch.Info, error) { return test.p, test.err }); got != test.want {
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
			read := func(pid int) (procwatch.Info, error) {
				name, parent := "sh", 99
				if pid == 99 {
					name, parent = test.name, 1
				}
				return procwatch.Info{PID: pid, PPID: parent, Name: name, Start: "start", UID: uint32(os.Getuid())}, nil
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
	if ref := ownerFrom((stubCursorAdapter{}).Descriptor(), 100, func(int) (procwatch.Info, error) { return procwatch.Info{}, syscall.EPERM }); ref != nil {
		t.Fatal("invented an owner without OS evidence")
	}
}

func TestProcessFingerprintTracksRealExit(t *testing.T) {
	selfProcess(t)
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	p, err := procwatch.Read(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ref := &model.ProcessRef{PID: p.PID, Start: p.Start, Role: "client"}
	if processLiveness(ref) != "alive" {
		t.Fatal("live child not identified")
	}
	store := fixtureStore(t)
	session, key := register(t, store, "cursor")
	armedLease(t, store, session.ID, key)
	current, err := store.session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Owner = ref
	if err := store.saveSession(current); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if processLiveness(ref) != "exited" {
		t.Fatal("exited child remained alive")
	}
	if _, err := store.Prune(false, nil); err != nil {
		t.Fatal(err)
	}
	current, err = store.session(session.ID)
	if err != nil || !current.Retired || current.Listener != nil {
		t.Fatal("positive owner exit did not retire the session and release its lease", err)
	}

}
