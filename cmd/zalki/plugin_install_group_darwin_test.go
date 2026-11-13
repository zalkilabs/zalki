//go:build darwin

package main

import (
	"encoding/binary"
	"math"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The caller exclusively owns an unreaped cmd.Process. Darwin Getpgid omits
// zombies; kern.proc.pid still verifies that owned child and its actual PGID.
func readPTYInstallGroup(pid int) (int, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if int(info.Proc.P_pid) != pid || int(info.Eproc.Ppid) != os.Getpid() || int(info.Eproc.Ucred.Uid) != os.Getuid() {
		return 0, syscall.EPERM
	}
	return int(info.Eproc.Pgid), nil
}

var ptyInstallKinfoLayout unix.KinfoProc

const (
	ptyInstallProcOffset   = unsafe.Offsetof(ptyInstallKinfoLayout.Proc)
	ptyInstallEprocOffset  = unsafe.Offsetof(ptyInstallKinfoLayout.Eproc)
	ptyInstallPIDOffset    = ptyInstallProcOffset + unsafe.Offsetof(ptyInstallKinfoLayout.Proc.P_pid)
	ptyInstallStateOffset  = ptyInstallProcOffset + unsafe.Offsetof(ptyInstallKinfoLayout.Proc.P_stat)
	ptyInstallParentOffset = ptyInstallEprocOffset + unsafe.Offsetof(ptyInstallKinfoLayout.Eproc.Ppid)
	ptyInstallGroupOffset  = ptyInstallEprocOffset + unsafe.Offsetof(ptyInstallKinfoLayout.Eproc.Pgid)
	ptyInstallUIDOffset    = ptyInstallEprocOffset + unsafe.Offsetof(ptyInstallKinfoLayout.Eproc.Ucred) + unsafe.Offsetof(ptyInstallKinfoLayout.Eproc.Ucred.Uid)
)

// A single-attempt complete census must include the same unreaped owned leader.
// Unlike SysctlKinfoProcSlice, SysctlRaw does not retry ENOMEM indefinitely.
// Generated ABI offsets only; no kernel pointer field is dereferenced.
var queryPTYInstallGroup = unix.SysctlRaw

var verifyPTYInstallGroup = readPTYInstallGroup

func ptyInstallGroupExited(pid int) bool {
	return awaitPTYInstallGroupExit(pid, 250*time.Millisecond)
}

// Darwin can refuse a dying singleton before its visible state reaches SZOMB.
// Only that complete owned SRUN singleton may be polled; success still requires
// an actual all-zombie census. There is no further signal and no Wait here.
func awaitPTYInstallGroupExit(pid int, limit time.Duration) bool {
	end := time.Now().Add(limit)
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if !time.Now().Before(end) {
			return false
		}
		group, err := verifyPTYInstallGroup(pid)
		if err != nil || group != pid {
			return false
		}
		raw, err := queryPTYInstallGroup("kern.proc.pgrp", pid)
		if ptyInstallZombieGroup(pid, raw, err) {
			return time.Now().Before(end)
		}
		if err != nil || !ptyInstallLeaderTransition(pid, raw) {
			return false
		}
		select {
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

func ptyInstallLeaderTransition(pid int, raw []byte) bool {
	if pid <= 1 || len(raw) != unix.SizeofKinfoProc || raw[ptyInstallStateOffset] != 2 {
		return false
	}
	read := func(field uintptr) uint32 { return binary.NativeEndian.Uint32(raw[field : field+4]) }
	return int(read(ptyInstallPIDOffset)) == pid && int(read(ptyInstallParentOffset)) == os.Getpid() && int(read(ptyInstallUIDOffset)) == os.Getuid() && int(read(ptyInstallGroupOffset)) == pid
}

func ptyInstallZombieGroup(pid int, raw []byte, queryErr error) bool {
	if queryErr != nil || len(raw) == 0 || len(raw)%unix.SizeofKinfoProc != 0 || len(raw)/unix.SizeofKinfoProc > 4096 {
		return false
	}
	seen := map[uint32]bool{}
	leader := false
	for offset := 0; offset < len(raw); offset += unix.SizeofKinfoProc {
		member := raw[offset : offset+unix.SizeofKinfoProc]
		read := func(field uintptr) uint32 { return binary.NativeEndian.Uint32(member[field : field+4]) }
		child, parent, group := read(ptyInstallPIDOffset), read(ptyInstallParentOffset), read(ptyInstallGroupOffset)
		if !validPTYInstallProcessNumbers(child, parent) || seen[child] || member[ptyInstallStateOffset] != 5 || int(group) != pid || int(read(ptyInstallUIDOffset)) != os.Getuid() {
			return false
		}
		seen[child] = true
		if int(child) == pid {
			if int(parent) != os.Getpid() {
				return false
			}
			leader = true
		}
	}
	return leader
}

func TestPTYInstallZombieGroupProof(t *testing.T) {
	pid := 10001
	raw := make([]byte, unix.SizeofKinfoProc)
	set := func(b []byte, field uintptr, value uint32) { binary.NativeEndian.PutUint32(b[field:field+4], value) }
	set(raw, ptyInstallPIDOffset, uint32(pid))
	set(raw, ptyInstallParentOffset, uint32(os.Getpid()))
	set(raw, ptyInstallGroupOffset, uint32(pid))
	set(raw, ptyInstallUIDOffset, uint32(os.Getuid()))
	raw[ptyInstallStateOffset] = 5
	if !ptyInstallZombieGroup(pid, raw, nil) {
		t.Fatal("owned unreaped PTY zombie leader was not accepted")
	}
	live := append([]byte(nil), raw...)
	live[ptyInstallStateOffset] = 2
	missing := append([]byte(nil), raw...)
	set(missing, ptyInstallPIDOffset, uint32(pid+1))
	changed := append([]byte(nil), raw...)
	set(changed, ptyInstallParentOffset, uint32(os.Getpid()+1))
	tests := []struct {
		name string
		raw  []byte
		err  error
	}{
		{"empty", nil, nil}, {"truncated", raw[:len(raw)-1], nil}, {"live", live, nil}, {"missing_leader", missing, nil}, {"changed_parent", changed, nil}, {"duplicate", append(append([]byte(nil), raw...), raw...), nil}, {"query_failure", raw, syscall.EIO}, {"ENOMEM", raw, syscall.ENOMEM},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if ptyInstallZombieGroup(pid, test.raw, test.err) {
				t.Fatal("PTY zombie proof accepted incomplete or unowned group", test.name)
			}
		})
	}
}

func TestPTYInstallZombieTransition(t *testing.T) {
	pid := 10001
	raw := make([]byte, unix.SizeofKinfoProc)
	set := func(field uintptr, value uint32) { binary.NativeEndian.PutUint32(raw[field:field+4], value) }
	set(ptyInstallPIDOffset, uint32(pid))
	set(ptyInstallParentOffset, uint32(os.Getpid()))
	set(ptyInstallGroupOffset, uint32(pid))
	set(ptyInstallUIDOffset, uint32(os.Getuid()))
	raw[ptyInstallStateOffset] = 2
	oldQuery, oldVerify := queryPTYInstallGroup, verifyPTYInstallGroup
	defer func() { queryPTYInstallGroup, verifyPTYInstallGroup = oldQuery, oldVerify }()
	verifyPTYInstallGroup = func(int) (int, error) { return pid, nil }
	t.Run("early_state2_then_zombie", func(t *testing.T) {
		calls := 0
		queryPTYInstallGroup = func(string, ...int) ([]byte, error) {
			calls++
			b := append([]byte(nil), raw...)
			if calls > 1 {
				b[ptyInstallStateOffset] = 5
			}
			return b, nil
		}
		if !ptyInstallGroupExited(pid) || calls != 2 {
			t.Fatal("owned exiting PTY leader was not observed as a zombie", calls)
		}
	})
	t.Run("never_zombie_is_bounded", func(t *testing.T) {
		calls := 0
		queryPTYInstallGroup = func(string, ...int) ([]byte, error) {
			calls++
			if calls > 40 {
				return nil, syscall.EIO
			}
			return raw, nil
		}
		start := time.Now()
		accepted := awaitPTYInstallGroupExit(pid, 20*time.Millisecond)
		if accepted || time.Since(start) > 150*time.Millisecond {
			t.Fatal("PTY zombie wait exceeded cleanup deadline", calls, time.Since(start))
		}
	})
	checkPTYTransitionQueryFailures(t, pid, raw)
	checkPTYTransitionAuthority(t, pid, raw)
}

func checkPTYTransitionQueryFailures(t *testing.T, pid int, raw []byte) {
	t.Helper()
	for _, queryErr := range []error{syscall.EIO, syscall.ENOMEM} {
		t.Run(queryErr.Error(), func(t *testing.T) {
			calls := 0
			queryPTYInstallGroup = func(string, ...int) ([]byte, error) { calls++; return raw, queryErr }
			if ptyInstallGroupExited(pid) || calls != 1 {
				t.Fatal("PTY transition retried a failed kernel query", calls)
			}
		})
	}

}

func checkPTYTransitionAuthority(t *testing.T, pid int, raw []byte) {
	t.Helper()

	t.Run("extra_live_member", func(t *testing.T) {
		calls := 0
		member := append([]byte(nil), raw...)
		binary.NativeEndian.PutUint32(member[ptyInstallPIDOffset:], uint32(pid+1))
		queryPTYInstallGroup = func(string, ...int) ([]byte, error) {
			calls++
			return append(append([]byte(nil), raw...), member...), nil
		}
		if ptyInstallGroupExited(pid) || calls != 1 {
			t.Fatal("PTY transition waited on an extra live member", calls)
		}
	})
	t.Run("changed_group", func(t *testing.T) {
		calls := 0
		queryPTYInstallGroup = func(string, ...int) ([]byte, error) { calls++; return raw, nil }
		verifyPTYInstallGroup = func(int) (int, error) { return pid + 1, nil }
		if ptyInstallGroupExited(pid) || calls != 0 {
			t.Fatal("PTY transition continued after group identity changed", calls)
		}
		verifyPTYInstallGroup = func(int) (int, error) { return pid, nil }
	})
	t.Run("identity_loss", func(t *testing.T) {
		calls := 0
		queryPTYInstallGroup = func(string, ...int) ([]byte, error) { calls++; return raw, nil }
		verifyPTYInstallGroup = func(int) (int, error) { return 0, syscall.EIO }
		if ptyInstallGroupExited(pid) || calls != 0 {
			t.Fatal("PTY transition continued after identity loss", calls)
		}
	})
}

func validPTYInstallProcessNumbers(child, parent uint32) bool {
	return child > 1 && child <= math.MaxInt32 && parent > 0 && parent <= math.MaxInt32
}
