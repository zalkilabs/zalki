//go:build linux

package main

import "syscall"

func readPTYInstallGroup(pid int) (int, error) { return syscall.Getpgid(pid) }
func ptyInstallGroupExited(int) bool           { return false }
