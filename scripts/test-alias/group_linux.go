//go:build linux

package main

import "syscall"

// EPERM remains an error on Linux; no group-enumeration fallback is used.
func aliasGroupExited(aliasChildRef) bool { return false }

func readAliasProcessGroup(pid int) (int, error) { return syscall.Getpgid(pid) }
