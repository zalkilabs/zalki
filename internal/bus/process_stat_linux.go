package bus

import "syscall"

// ChangedNS returns the inode change time in nanoseconds.
func ChangedNS(stat *syscall.Stat_t) int64 { return stat.Ctim.Nano() }
