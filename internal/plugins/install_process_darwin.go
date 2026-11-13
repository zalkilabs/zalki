//go:build darwin

package plugins

import (
	"encoding/binary"
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Only this still-unreaped group's snapshot is queried. SysctlRaw makes one
// size/read attempt: ENOMEM, malformed length or any other failure stays closed.
var readInstallBuildGroup = unix.SysctlRaw

func installBuildGroupExited(ref installBuildChildRef) bool {
	raw, err := readInstallBuildGroup("kern.proc.pgrp", ref.identity.PID)
	return installBuildZombieGroup(ref, raw, err)
}

type installBuildGroupMember struct {
	pid, parent, group int
	uid                uint32
	state              byte
	start              string
}

// Offsets come only from x/sys's generated ABI type. No pointer conversion or
// dereference of kernel pointer fields is performed.
var installBuildKinfoLayout unix.KinfoProc

const (
	installBuildProcOffset    = unsafe.Offsetof(installBuildKinfoLayout.Proc)
	installBuildEprocOffset   = unsafe.Offsetof(installBuildKinfoLayout.Eproc)
	installBuildPIDOffset     = installBuildProcOffset + unsafe.Offsetof(installBuildKinfoLayout.Proc.P_pid)
	installBuildStateOffset   = installBuildProcOffset + unsafe.Offsetof(installBuildKinfoLayout.Proc.P_stat)
	installBuildStartOffset   = installBuildProcOffset + unsafe.Offsetof(installBuildKinfoLayout.Proc.P_starttime)
	installBuildSecondsOffset = installBuildStartOffset + unsafe.Offsetof(installBuildKinfoLayout.Proc.P_starttime.Sec)
	installBuildMicrosOffset  = installBuildStartOffset + unsafe.Offsetof(installBuildKinfoLayout.Proc.P_starttime.Usec)
	installBuildParentOffset  = installBuildEprocOffset + unsafe.Offsetof(installBuildKinfoLayout.Eproc.Ppid)
	installBuildGroupOffset   = installBuildEprocOffset + unsafe.Offsetof(installBuildKinfoLayout.Eproc.Pgid)
	installBuildUIDOffset     = installBuildEprocOffset + unsafe.Offsetof(installBuildKinfoLayout.Eproc.Ucred) + unsafe.Offsetof(installBuildKinfoLayout.Eproc.Ucred.Uid)
)

func installBuildZombieGroup(ref installBuildChildRef, raw []byte, queryErr error) bool {
	if queryErr != nil || !ref.valid(ref.identity.PID) || len(raw) == 0 || len(raw)%unix.SizeofKinfoProc != 0 || len(raw)/unix.SizeofKinfoProc > 4096 {
		return false
	}
	seen := map[int]bool{}
	leader := false
	for offset := 0; offset < len(raw); offset += unix.SizeofKinfoProc {
		member, ok := decodeInstallBuildMember(raw[offset : offset+unix.SizeofKinfoProc])
		if !ok || seen[member.pid] || member.state != 5 || member.uid != ref.uid || member.group != ref.identity.PID {
			return false
		}
		seen[member.pid] = true
		if member.pid == ref.identity.PID {
			if member.parent != ref.parent || member.start != ref.identity.Start {
				return false
			}
			leader = true
		}
	}
	return leader
}

func decodeInstallBuildMember(raw []byte) (installBuildGroupMember, bool) {
	var member installBuildGroupMember
	if len(raw) != unix.SizeofKinfoProc {
		return member, false
	}
	read32 := func(offset uintptr) uint32 { return binary.NativeEndian.Uint32(raw[offset : offset+4]) }
	pid, parent, group := read32(installBuildPIDOffset), read32(installBuildParentOffset), read32(installBuildGroupOffset)
	seconds := binary.NativeEndian.Uint64(raw[installBuildSecondsOffset : installBuildSecondsOffset+8])
	micros := read32(installBuildMicrosOffset)
	if pid <= 1 || pid > math.MaxInt32 || parent == 0 || parent > math.MaxInt32 || group <= 1 || group > math.MaxInt32 || seconds == 0 || seconds > math.MaxInt64 || micros >= 1000000 {
		return member, false
	}
	member = installBuildGroupMember{pid: int(pid), parent: int(parent), group: int(group), uid: read32(installBuildUIDOffset), state: raw[installBuildStateOffset], start: fmt.Sprintf("%d.%06d", seconds, micros)}
	return member, true
}

func readInstallBuildProcessGroup(pid int) (int, error) {
	raw, err := unix.SysctlRaw("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	member, ok := decodeInstallBuildMember(raw)
	if !ok || member.pid != pid {
		return 0, unix.EIO
	}
	return member.group, nil
}
