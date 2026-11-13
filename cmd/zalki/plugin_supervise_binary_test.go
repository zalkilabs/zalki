package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// S21: a changed executable is a handover; a missing one is retried for
// the grace and only then gone; one that comes back unchanged resumes.
func TestPluginSupervisorBinaryCheck(t *testing.T) {
	start := binaryIdentity{dev: 1, ino: 2, size: 3, modified: 4}
	now := stateEpoch
	var current binaryIdentity
	var missing bool
	s := &supervisor{binaryID: start, now: func() time.Time { return now }, resolve: func(string) (binaryIdentity, error) {
		if missing {
			return binaryIdentity{}, os.ErrNotExist
		}
		return current, nil
	}}
	current = start
	if s.checkBinary() != binarySame {
		t.Fatal("an unchanged executable was reported as changed")
	}
	missing = true
	if s.checkBinary() != binaryMissing {
		t.Fatal("a missing executable was not retried")
	}
	now = now.Add(binaryMissingGrace - time.Second)
	if s.checkBinary() != binaryMissing {
		t.Fatal("a missing executable was given up before the grace")
	}
	missing = false
	if s.checkBinary() != binarySame || s.missingSince != nil {
		t.Fatal("an executable that came back unchanged did not resume")
	}
	missing = true
	s.checkBinary()
	now = now.Add(binaryMissingGrace)
	if s.checkBinary() != binaryGone {
		t.Fatal("an executable missing past the grace was not gone")
	}
	missing, current = false, binaryIdentity{dev: 1, ino: 9, size: 3, modified: 4}
	if s.checkBinary() != binaryChanged {
		t.Fatal("a replaced executable was not a handover")
	}
}

func copyExecutable(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err == nil {
		_, err = io.Copy(out, in)
		err = errors.Join(err, out.Close())
	}
	if err != nil {
		t.Fatal(err)
	}
}

// S21/S22: a supervisor started through a stable link exits 75 when the
// link moves to a new build, after stopping its hosts; the stop is not
// counted. With no plugins folder it exits 0 at once.
func TestPluginSupervisorBinaryReplacement(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := supervisorFixture(t, binary, "alpha")
	bin := t.TempDir()
	copyExecutable(t, binary, filepath.Join(bin, "zalki-1"))
	copyExecutable(t, binary, filepath.Join(bin, "zalki-2"))
	stable := filepath.Join(bin, "zalki")
	if err := os.Symlink(filepath.Join(bin, "zalki-1"), stable); err != nil {
		t.Fatal(err)
	}
	_, done := startSupervisor(t, stable, state)
	run := awaitRunState(t, state, "alpha", running)
	if err := os.Symlink(filepath.Join(bin, "zalki-2"), stable+".new"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stable+".new", stable); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != exitReplaced {
			t.Fatalf("a replaced executable did not exit %d: %v", exitReplaced, err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the supervisor did not notice the new executable")
	}
	after := readRunState(t, state, "alpha")
	if !hostProcessGone(run.PID) || !hostProcessGone(run.PluginPID) || after.State != runStopped || after.ReasonCode != "zalki_replaced" || len(after.Restarts) != 0 {
		t.Fatalf("handover left %s/%s, %d failures", after.State, after.ReasonCode, len(after.Restarts))
	}
	empty := exec.CommandContext(t.Context(), binary, "--state", t.TempDir(), "plugins", "supervise")
	empty.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	if err := empty.Run(); err != nil {
		t.Fatal("a state without plugins did not exit 0", err)
	}
}
