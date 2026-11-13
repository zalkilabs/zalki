//go:build linux

package plugins

import "syscall"

func changedNS(stat *syscall.Stat_t) int64 { return stat.Ctim.Nano() }

// Linux device fields are unsigned; widen smaller fields without a suppression.
func deviceNumber(stat *syscall.Stat_t) uint64 {
	return uint64(stat.Dev)
}
