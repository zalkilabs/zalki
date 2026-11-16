//go:build darwin

package main

// statDevice is the same conversion as the private deviceNumber in
// internal/plugins; cmd/zalki cannot call that one without a new export.

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// statDevice: a Darwin dev_t is int32 and never negative.
func statDevice(stat *syscall.Stat_t) uint64 { return uint64(stat.Dev) } //nolint:gosec // G115: a device number is never negative.

// pgrpQuery reads the process group into ONE fixed kinfo_proc row: the
// capacity is set before the read, and the kernel answers ENOMEM, writing
// nothing more, when the group has more than one member. One attempt, no
// retry, no allocation from a kernel-reported size. Tests replace it.
var pgrpQuery = func(pgid int) (*unix.KinfoProc, error) { return unix.SysctlKinfoProc("kern.proc.pgrp", pgid) }

// groupOnlyZombies is positive kernel evidence that the group is exactly its
// leader and that the leader has exited and is not yet reaped. Darwin answers
// a group signal to such a group with EPERM. A larger group (ENOMEM), any
// other error or another row is unknown: false.
func groupOnlyZombies(pgid int) bool {
	row, err := pgrpQuery(pgid)
	return err == nil && row != nil && int(row.Proc.P_pid) == pgid && row.Proc.P_stat == 5 // SZOMB, sys/proc.h
}

// leaderExited reports a zombie (exited, not reaped) pid from ONE fixed-size
// sysctl row: the capacity is set before the read, with no retry loop, so it
// cannot spin or allocate inside the banner watchdog.
func leaderExited(pid int) bool {
	row, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && row != nil && int(row.Proc.P_pid) == pid && row.Proc.P_stat == 5 // SZOMB
}
