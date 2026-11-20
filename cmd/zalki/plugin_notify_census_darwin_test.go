//go:build darwin

package main

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The zombie-only proof reads ONE fixed kinfo_proc row and fails
// closed on an error (even with a row), a live leader or another pid.
func TestPluginNotificationGroupCensusIsBounded(t *testing.T) {
	old := pgrpQuery
	t.Cleanup(func() { pgrpQuery = old })
	row := func(pid int32, stat int8) *unix.KinfoProc {
		var r unix.KinfoProc
		r.Proc.P_pid, r.Proc.P_stat = pid, stat
		return &r
	}
	for _, tc := range []struct {
		name string
		row  *unix.KinfoProc
		err  error
		want bool
		says string
	}{
		{"zombie leader", row(100, 5), nil, true, "a zombie-only group was not recognized"},
		{"error with a row", row(100, 5), syscall.ENOMEM, false, "a census error was read as a zombie-only group"},
		{"live leader", row(100, 2), nil, false, "a live member was read as a zombie-only group"},
		{"other pid", row(101, 5), nil, false, "a census without the leader was read as a zombie-only group"},
		{"no row", nil, nil, false, "an empty census was read as a zombie-only group"},
	} {
		r, err := tc.row, tc.err
		pgrpQuery = func(int) (*unix.KinfoProc, error) { return r, err }
		if got := groupOnlyZombies(100); got != tc.want {
			t.Fatalf("%s: %s", tc.name, tc.says)
		}
	}
}

// For a real group with two members the census is refused
// by the kernel (ENOMEM into its one fixed row), never read in full.
func TestPluginNotificationGroupCensusAdmission(t *testing.T) {
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sh", "-c", "/bin/sleep 2 & /bin/sleep 2")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }() // both members end by themselves in 2 s
	time.Sleep(300 * time.Millisecond)
	row, err := pgrpQuery(cmd.Process.Pid)
	if !errors.Is(err, unix.ENOMEM) || row != nil {
		t.Fatalf("an oversize census was read instead of refused: %v", err)
	}
	if groupOnlyZombies(cmd.Process.Pid) {
		t.Fatal("a two-member live group was read as zombie-only")
	}
}
