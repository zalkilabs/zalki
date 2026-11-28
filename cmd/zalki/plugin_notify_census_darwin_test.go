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

type censusStep struct {
	row *unix.KinfoProc
	err error
}

// scriptCensus answers the group census with the steps in turn (the last one
// again and again) and shortens the gap between reads. It returns the number
// of reads so far.
func scriptCensus(t *testing.T, steps ...censusStep) *int {
	t.Helper()
	oldQuery, oldGap := pgrpQuery, groupCensusGap
	t.Cleanup(func() { pgrpQuery, groupCensusGap = oldQuery, oldGap })
	groupCensusGap = time.Millisecond
	reads := new(int)
	pgrpQuery = func(int) (*unix.KinfoProc, error) {
		step := steps[min(*reads, len(steps)-1)]
		*reads++
		return step.row, step.err
	}
	return reads
}

func censusRow(pid int32, stat int8) *unix.KinfoProc {
	var r unix.KinfoProc
	r.Proc.P_pid, r.Proc.P_stat = pid, stat
	return &r
}

// A group that is still leaving shows a running row, or more than one
// member, for a moment. The census is read again, up to a fixed number of
// times, and only a zombie leader alone is proof. At the bound the answer is
// unknown, whatever the last row said.
func TestPluginNotificationGroupCensusIsReadAgainWhileTheGroupLeaves(t *testing.T) {
	running, zombie := censusStep{censusRow(100, 2), nil}, censusStep{censusRow(100, 5), nil}
	crowded := censusStep{nil, unix.ENOMEM}
	for _, tc := range []struct {
		name  string
		steps []censusStep
		want  bool
		reads int // 0: the whole bound
	}{
		{"a zombie at once", []censusStep{zombie}, true, 1},
		{"a running row, then a zombie", []censusStep{running, running, zombie}, true, 3},
		{"more than one member, then a zombie", []censusStep{crowded, zombie}, true, 2},
		{"a leader that never leaves", []censusStep{running}, false, 0},
		{"a group that stays crowded", []censusStep{crowded}, false, 0},
		{"another error", []censusStep{{nil, syscall.EIO}}, false, 0},
		{"another pid", []censusStep{{censusRow(101, 5), nil}}, false, 0},
		{"no row", []censusStep{{nil, nil}}, false, 0},
		{"a zombie after the bound", append(make([]censusStep, groupCensusTries), zombie), false, 0},
	} {
		reads := scriptCensus(t, tc.steps...)
		began := time.Now()
		got := awaitGroupOnlyZombies(100)
		if got != tc.want {
			t.Fatalf("%s: answered %v", tc.name, got)
		}
		want := tc.reads
		if want == 0 {
			want = groupCensusTries
		}
		if *reads != want {
			t.Fatalf("%s: the census was read %d times, want %d", tc.name, *reads, want)
		}
		if time.Since(began) < time.Duration(want-1)*groupCensusGap {
			t.Fatalf("%s: the reads were not spaced by the gap", tc.name)
		}
	}
}

// The group signal is sent once. Reading the census again never signals
// again: a signal after the leader is reaped could reach a reused group.
func TestPluginNotificationEndOfGroupReadsTheCensusWithoutNewSignals(t *testing.T) {
	signals := 0
	old := notifyKill
	notifyKill = func(int, syscall.Signal) error { signals++; return syscall.EPERM }
	t.Cleanup(func() { notifyKill = old })
	reads := scriptCensus(t, censusStep{censusRow(100, 2), nil}, censusStep{censusRow(100, 5), nil})
	if !endNotifyGroup(100) || signals != 1 || *reads != 2 {
		t.Fatalf("a leaving group was not proven ended after one signal: signals %d, reads %d", signals, *reads)
	}
	signals = 0
	reads = scriptCensus(t, censusStep{censusRow(100, 2), nil})
	if endNotifyGroup(100) || signals != 1 || *reads != groupCensusTries {
		t.Fatalf("a group that stays running was read as ended or signalled again: signals %d, reads %d", signals, *reads)
	}
}
