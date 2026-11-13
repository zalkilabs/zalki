package plugins

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

func TestInstallBuildProcessChild(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "install-process-child" {
		t.Skip("subprocess helper")
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

type ownedBuildProcess struct {
	*exec.Cmd
	once sync.Once
	done chan struct{}
	err  error
}

func (c *ownedBuildProcess) runWait(run func() error) error {
	c.once.Do(func() { go func() { c.err = run(); close(c.done) }() })
	<-c.done
	return c.err
}
func (c *ownedBuildProcess) wait() error { return c.runWait(c.Wait) }

func ownedBuildChild(t *testing.T, group int) (*ownedBuildProcess, io.WriteCloser) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), binary, "-test.run=^TestInstallBuildProcessChild$", "--", "install-process-child")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &ownedBuildProcess{Cmd: cmd, done: make(chan struct{})}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = child.wait() })
	return child, in
}

func TestInstallBuildReapedLeaderNeverSignalsGroup(t *testing.T) {
	leader, in := ownedBuildChild(t, 0)
	member, _ := ownedBuildChild(t, leader.Process.Pid)
	_ = in.Close()
	if err := leader.wait(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- member.wait() }()
	_ = stopInstallBuildChild(leader.Cmd, installBuildChildRef{})
	select {
	case err := <-done:
		t.Fatal("late build cleanup killed the reaped leader's live group member", err)
	case <-time.After(150 * time.Millisecond):
	}
	_ = member.Process.Kill()
	<-done
}

func TestInstallBuildDirectKillPrecedesWait(t *testing.T) {
	for _, groupErr := range []error{nil, syscall.ESRCH, syscall.EIO} {
		t.Run(errorName(groupErr), func(t *testing.T) {
			cmd, _ := ownedBuildChild(t, 0)
			originalObserve, originalSignal := observeInstallBuildProcess, signalInstallBuildGroup
			defer func() { observeInstallBuildProcess = originalObserve; signalInstallBuildGroup = originalSignal }()
			observeInstallBuildProcess = func(context.Context, int) (installBuildChildRef, error) {
				info, err := procwatch.Read(cmd.Process.Pid)
				if err != nil {
					return installBuildChildRef{}, err
				}
				return installBuildChildRef{identity: procwatch.Identity{PID: info.PID, Start: info.Start}, parent: info.PPID, uid: info.UID}, context.Canceled
			}
			signalInstallBuildGroup = func(int, syscall.Signal) error { return groupErr }
			// A fault omitting direct Kill is bounded independently of WaitDelay.
			timer := time.AfterFunc(2*time.Second, func() { _ = cmd.Process.Kill() })
			defer timer.Stop()
			started := time.Now()
			err := cmd.runWait(func() error { return finishInstallBuildChild(t.Context(), cmd.Cmd, nil) })
			if time.Since(started) > time.Second {
				t.Fatal("direct original-child kill was omitted before Wait")
			}
			if cmd.ProcessState == nil {
				t.Fatal("original child was not reaped")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation lost", err)
			}
			if groupErr != nil && !errors.Is(groupErr, syscall.ESRCH) && !errors.Is(err, groupErr) {
				t.Fatal("group signal failure lost", err)
			}
		})
	}
}

func errorName(err error) string {
	if err == nil {
		return "success"
	}
	return err.Error()
}

func TestInstallBuildIdentityLossSuppressesGroup(t *testing.T) {
	cmd, _ := ownedBuildChild(t, 0)
	originalObserve, originalSignal := observeInstallBuildProcess, signalInstallBuildGroup
	defer func() { observeInstallBuildProcess = originalObserve; signalInstallBuildGroup = originalSignal }()
	observeInstallBuildProcess = func(context.Context, int) (installBuildChildRef, error) { return installBuildChildRef{}, syscall.EIO }
	calls := 0
	signalInstallBuildGroup = func(int, syscall.Signal) error { calls++; return nil }
	err := cmd.runWait(func() error { return finishInstallBuildChild(t.Context(), cmd.Cmd, nil) })
	if calls != 0 {
		t.Fatal("identity loss permitted a group signal")
	}
	if !errors.Is(err, syscall.EIO) || cmd.ProcessState == nil {
		t.Fatal("observation failure or sole reaping lost", err)
	}
}

func TestInstallBuildCancellationKeepsUnreapedAuthority(t *testing.T) {
	cmd, _ := ownedBuildChild(t, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ref, err := observeInstallBuildChild(ctx, cmd.Process.Pid)
	if !errors.Is(err, context.Canceled) || !ref.valid(cmd.Process.Pid) {
		t.Fatal("cancellation discarded verified unreaped authority", ref, err)
	}
	if cmd.ProcessState != nil {
		t.Fatal("observation reaped the child")
	}
	if err := stopInstallBuildChild(cmd.Cmd, ref); err != nil {
		t.Fatal(err)
	}
	_ = killInstallBuildChild(cmd.Process)
	_ = cmd.wait()
}

func TestInstallBuildIdentityChangesRefuseSignal(t *testing.T) {
	cmd, _ := ownedBuildChild(t, 0)
	info, err := procwatch.Read(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ref := installBuildChildRef{identity: procwatch.Identity{PID: info.PID, Start: info.Start}, parent: info.PPID, uid: info.UID}
	read, signal := readInstallBuildProcess, signalInstallBuildGroup
	defer func() { readInstallBuildProcess = read; signalInstallBuildGroup = signal }()
	readInstallBuildProcess = func(int) (procwatch.Info, error) { changed := info; changed.Start += "changed"; return changed, nil }
	calls := 0
	signalInstallBuildGroup = func(int, syscall.Signal) error { calls++; return nil }
	err = stopInstallBuildChild(cmd.Cmd, ref)
	if calls != 0 || err == nil || !strings.Contains(err.Error(), "identity unavailable") {
		t.Fatal("changed identity permitted a group signal", err)
	}
}

func TestInstallBuildFixturePIDNeverSignals(t *testing.T) {
	member, _ := ownedBuildChild(t, 0)
	done := make(chan error, 1)
	go func() { done <- member.wait() }()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(member.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stopBuildOutputFixture(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatal("recorded fixture PID was used as signal authority", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(dir, "stop-child")); err != nil {
		t.Fatal("cooperative stop marker missing", err)
	}
	_ = member.Process.Kill()
	<-done
}

type failedBuildOutput struct{ err error }

func (w failedBuildOutput) Write([]byte) (int, error) { return 0, w.err }

func TestInstallBuildOutputFailureStopsBeforeWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "/bin/sh", "-c", "printf output; sleep 10")
	cmd.Cancel = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	want := errors.New("private output failure")
	cmd.Stdout = failedBuildOutput{want}
	start := time.Now()
	err := runInstallBuildChild(ctx, cmd)
	if time.Since(start) > time.Second {
		t.Fatal("output failure did not stop the live child before Wait", err)
	}
	if !errors.Is(err, want) || cmd.ProcessState == nil {
		t.Fatal("output failure or reaping lost", err)
	}
}

func TestInstallBuildCanceledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "/bin/sh", "-c", "exit 0")
	cmd.Stdout = io.Discard
	err := runInstallBuildChild(ctx, cmd)
	if !errors.Is(err, context.Canceled) || cmd.Process != nil {
		t.Fatal("canceled build was started", err)
	}
}

// The signal seam leaves a real owned member alive so a regression that adds
// the old raw group kill after finish's Wait has an observable victim.
func TestInstallBuildFinishNeverSignalsAfterReaping(t *testing.T) {
	leader, in := ownedBuildChild(t, 0)
	member, _ := ownedBuildChild(t, leader.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- member.wait() }()
	_ = in.Close()
	old := signalInstallBuildGroup
	defer func() { signalInstallBuildGroup = old }()
	calls := 0
	afterWait := false
	signalInstallBuildGroup = func(int, syscall.Signal) error {
		calls++
		afterWait = leader.ProcessState != nil
		return nil
	}
	if err := leader.runWait(func() error { return finishInstallBuildChild(t.Context(), leader.Cmd, nil) }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || afterWait {
		t.Fatal("final build group signal was not before the sole Wait", calls, afterWait)
	}
	select {
	case err := <-done:
		t.Fatal("build finish signalled the live member after reaping its leader", err)
	case <-time.After(150 * time.Millisecond):
	}
	_ = member.Process.Kill()
	<-done
}
