package main

import (
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A stopped group whose only member is its exited, unreaped leader
// answers EPERM on Darwin; the wait ends at once on the zombie-only proof
// instead of the whole grace. The leader stays an owned child with one Wait.
func TestPluginStopWaitSeesZombieOnlyGroup(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/usr/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	wait := func() { once.Do(func() { _ = cmd.Wait() }) }
	t.Cleanup(wait)
	pid := cmd.Process.Pid
	until := time.Now().Add(2 * time.Second)
	for !leaderExited(pid) {
		if time.Now().After(until) {
			t.Fatal("the leader did not become a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	waitGroupGone(pid, groupGrace)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("a zombie-only group was waited for the full grace: %v", elapsed)
	}
	wait()
}
