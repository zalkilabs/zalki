//go:build darwin

package main

// statDevice is the same conversion as the private deviceNumber in
// internal/plugins; cmd/zalki cannot call that one without a new export.

import "syscall"

// statDevice: a Darwin dev_t is int32 and never negative.
func statDevice(stat *syscall.Stat_t) uint64 { return uint64(stat.Dev) } //nolint:gosec // G115: a device number is never negative.
