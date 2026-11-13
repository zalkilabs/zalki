//go:build linux

package plugins

import "syscall"

// EPERM remains an error on Linux; no group-enumeration fallback is used.
func installBuildGroupExited(installBuildChildRef) bool { return false }

func readInstallBuildProcessGroup(pid int) (int, error) { return syscall.Getpgid(pid) }
