package plugins

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

type ownedSignalCleanupProcess struct {
	*exec.Cmd
	once sync.Once
	done chan struct{}
	err  error
}

func (c *ownedSignalCleanupProcess) wait() error {
	c.once.Do(func() { go func() { c.err = c.Wait(); close(c.done) }() })
	<-c.done
	return c.err
}
func ownedSignalCleanupChild(t *testing.T, group int) (*ownedSignalCleanupProcess, io.WriteCloser) {
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
	child := &ownedSignalCleanupProcess{Cmd: cmd, done: make(chan struct{})}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = child.wait() })
	return child, in
}

func TestForegroundFixtureNeverSignalsAfterWaitStarts(t *testing.T) {
	leader, in := ownedSignalCleanupChild(t, 0)
	member, _ := ownedSignalCleanupChild(t, leader.Process.Pid)
	h := &signalFixtureHost{cmd: leader.Cmd, waitChild: leader.wait, done: make(chan struct{})}
	h.startWait()
	_ = in.Close()
	if err := h.wait(t); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- member.wait() }()
	_ = h.signalGroup(syscall.SIGKILL)
	select {
	case err := <-done:
		t.Fatal("foreground fixture signalled a group after Wait started", err)
	case <-time.After(150 * time.Millisecond):
	}
	_ = member.Process.Kill()
	<-done
}
