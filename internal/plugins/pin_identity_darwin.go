//go:build darwin

package plugins

import "syscall"

func changedNS(stat *syscall.Stat_t) int64 { return stat.Ctimespec.Nano() }

// Darwin's device field is signed, so keep its conversion and suppression here.
func deviceNumber(stat *syscall.Stat_t) uint64 {
	return uint64(stat.Dev) //nolint:gosec // G115: preserve the device number in the identity.
}
