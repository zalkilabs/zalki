//go:build darwin || linux

package main

import (
	"context"
	"github.com/zalkilabs/zalki/internal/procwatch"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

type ownedInstallCleanupProcess struct {
	*exec.Cmd
	once sync.Once
	done chan struct{}
	err  error
}

func (c *ownedInstallCleanupProcess) wait() error {
	c.once.Do(func() { go func() { c.err = c.Wait(); close(c.done) }() })
	<-c.done
	return c.err
}
func ownedInstallCleanupChild(t *testing.T, group int) (*ownedInstallCleanupProcess, io.WriteCloser) {
	t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/cat")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &ownedInstallCleanupProcess{Cmd: cmd, done: make(chan struct{})}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = child.wait() })
	return child, in
}

func reapedInstallLeaderWithMember(t *testing.T) (*ownedInstallCleanupProcess, *ownedInstallCleanupProcess, chan error) {
	t.Helper()
	leader, in := ownedInstallCleanupChild(t, 0)
	member, _ := ownedInstallCleanupChild(t, leader.Process.Pid)
	_ = in.Close()
	if err := leader.wait(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- member.wait() }()
	return leader, member, done
}

func TestPTYInstallCleanupNeverSignalsReapedLeader(t *testing.T) {
	leader, member, done := reapedInstallLeaderWithMember(t)
	stopPTYInstall(t, leader.Cmd)
	select {
	case err := <-done:
		t.Fatal("PTY cleanup signalled a reaped leader's live group", err)
	case <-time.After(150 * time.Millisecond):
	}
	_ = member.Process.Kill()
	<-done
}

func TestGatedBuildCleanupNeverSignalsRecordedGroup(t *testing.T) {
	leader, member, done := reapedInstallLeaderWithMember(t)
	dir := t.TempDir()
	b := &gatedBuild{pidFile: filepath.Join(dir, "pid"), stopFile: filepath.Join(dir, "stop")}
	if err := os.WriteFile(b.pidFile, []byte(strconv.Itoa(leader.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	b.stop(t)
	select {
	case err := <-done:
		t.Fatal("gated build cleanup signalled a recorded stale group", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := os.Stat(b.stopFile); err != nil {
		t.Fatal("private cooperative stop marker missing", err)
	}
	_ = member.Process.Kill()
	<-done
}

func TestPTYInstallCleanupStopsUnreapedExitedLeaderGroup(t *testing.T) {
	leader, in := ownedInstallCleanupChild(t, 0)
	member, _ := ownedInstallCleanupChild(t, leader.Process.Pid)
	_ = in.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		info, err := procwatch.Read(leader.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if info.Zombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PTY fixture leader did not exit without reaping")
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- member.wait() }()
	stopPTYInstallWith(t, leader.Cmd, leader.wait)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unreaped exited PTY leader left its live group member")
	}
}
