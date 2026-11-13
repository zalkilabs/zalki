//go:build darwin

package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Only this still-unreaped group's snapshot is queried. SysctlRaw makes one
// size/read attempt: ENOMEM, malformed length or any other failure stays closed.
var readAliasGroup = unix.SysctlRaw

func aliasGroupExited(ref aliasChildRef) bool {
	raw, err := readAliasGroup("kern.proc.pgrp", ref.identity.PID)
	return aliasZombieGroup(ref, raw, err)
}

type aliasGroupMember struct {
	pid, parent, group int
	uid                uint32
	state              byte
	start              string
}

// Offsets come only from x/sys's generated ABI type. No pointer conversion or
// dereference of kernel pointer fields is performed.
var aliasKinfoLayout unix.KinfoProc

const (
	aliasProcOffset    = unsafe.Offsetof(aliasKinfoLayout.Proc)
	aliasEprocOffset   = unsafe.Offsetof(aliasKinfoLayout.Eproc)
	aliasPIDOffset     = aliasProcOffset + unsafe.Offsetof(aliasKinfoLayout.Proc.P_pid)
	aliasStateOffset   = aliasProcOffset + unsafe.Offsetof(aliasKinfoLayout.Proc.P_stat)
	aliasStartOffset   = aliasProcOffset + unsafe.Offsetof(aliasKinfoLayout.Proc.P_starttime)
	aliasSecondsOffset = aliasStartOffset + unsafe.Offsetof(aliasKinfoLayout.Proc.P_starttime.Sec)
	aliasMicrosOffset  = aliasStartOffset + unsafe.Offsetof(aliasKinfoLayout.Proc.P_starttime.Usec)
	aliasParentOffset  = aliasEprocOffset + unsafe.Offsetof(aliasKinfoLayout.Eproc.Ppid)
	aliasGroupOffset   = aliasEprocOffset + unsafe.Offsetof(aliasKinfoLayout.Eproc.Pgid)
	aliasUIDOffset     = aliasEprocOffset + unsafe.Offsetof(aliasKinfoLayout.Eproc.Ucred) + unsafe.Offsetof(aliasKinfoLayout.Eproc.Ucred.Uid)
)

func aliasZombieGroup(ref aliasChildRef, raw []byte, queryErr error) bool {
	if queryErr != nil || !ref.valid(ref.identity.PID) || len(raw) == 0 || len(raw)%unix.SizeofKinfoProc != 0 || len(raw)/unix.SizeofKinfoProc > 4096 {
		return false
	}
	seen := map[int]bool{}
	leader := false
	for offset := 0; offset < len(raw); offset += unix.SizeofKinfoProc {
		member, ok := decodeAliasMember(raw[offset : offset+unix.SizeofKinfoProc])
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

func decodeAliasMember(raw []byte) (aliasGroupMember, bool) {
	var member aliasGroupMember
	if len(raw) != unix.SizeofKinfoProc {
		return member, false
	}
	read32 := func(offset uintptr) uint32 { return binary.NativeEndian.Uint32(raw[offset : offset+4]) }
	pid, parent, group := read32(aliasPIDOffset), read32(aliasParentOffset), read32(aliasGroupOffset)
	seconds := binary.NativeEndian.Uint64(raw[aliasSecondsOffset : aliasSecondsOffset+8])
	micros := read32(aliasMicrosOffset)
	if pid <= 1 || pid > math.MaxInt32 || parent == 0 || parent > math.MaxInt32 || group <= 1 || group > math.MaxInt32 || seconds == 0 || seconds > math.MaxInt64 || micros >= 1000000 {
		return member, false
	}
	member = aliasGroupMember{pid: int(pid), parent: int(parent), group: int(group), uid: read32(aliasUIDOffset), state: raw[aliasStateOffset], start: fmt.Sprintf("%d.%06d", seconds, micros)}
	return member, true
}

func readAliasProcessGroup(pid int) (int, error) {
	raw, err := unix.SysctlRaw("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	member, ok := decodeAliasMember(raw)
	if !ok || member.pid != pid {
		return 0, unix.EIO
	}
	return member.group, nil
}
