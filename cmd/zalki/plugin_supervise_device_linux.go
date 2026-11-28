//go:build linux

package main

import (
	"syscall"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

// statDevice returns Dev, which is already uint64 on Linux amd64 and arm64.
func statDevice(stat *syscall.Stat_t) uint64 { return stat.Dev }

// groupOnlyZombies: Linux answers a group signal to a zombie-only group with
// success, so an EPERM there is never read as exited.
func groupOnlyZombies(int) bool { return false }

// awaitGroupOnlyZombies has nothing to wait for on Linux: no proof exists.
func awaitGroupOnlyZombies(pgid int) bool { return groupOnlyZombies(pgid) }

// leaderExited reports a zombie (exited, not reaped) pid; /proc is one
// bounded file read.
func leaderExited(pid int) bool {
	info, err := procwatch.Read(pid)
	return err == nil && info.PID == pid && info.Zombie
}
