//go:build linux

package main

import "syscall"

// statDevice returns Dev, which is already uint64 on Linux amd64 and arm64.
func statDevice(stat *syscall.Stat_t) uint64 { return stat.Dev }
