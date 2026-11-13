//go:build darwin

package plugins

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/procwatch"
	"golang.org/x/sys/unix"
)

func TestInstallBuildZombieGroupProof(t *testing.T) {
	ref := installBuildChildRef{identity: procwatch.Identity{PID: 10001, Start: "123.000456"}, parent: os.Getpid(), uid: uint32(os.Getuid())}
	leader := installBuildGroupMember{pid: 10001, parent: ref.parent, group: 10001, uid: ref.uid, state: 5, start: "123.000456"}
	raw := encodeInstallBuildMember(leader)
	if !installBuildZombieGroup(ref, raw, nil) {
		t.Fatal("owned unreaped zombie leader was not accepted")
	}
	child := leader
	child.pid = 10002
	child.parent = 1
	if !installBuildZombieGroup(ref, append(append([]byte(nil), raw...), encodeInstallBuildMember(child)...), nil) {
		t.Fatal("complete zombie-only group was not accepted")
	}
	live := leader
	live.state = 1
	missing := leader
	missing.pid = 10002
	wrongParent := leader
	wrongParent.parent++
	wrongUID := leader
	wrongUID.uid++
	wrongGroup := leader
	wrongGroup.group++
	wrongStart := append([]byte(nil), raw...)
	binary.NativeEndian.PutUint64(wrongStart[installBuildSecondsOffset:installBuildSecondsOffset+8], 124)
	unknown := append([]byte(nil), raw...)
	binary.NativeEndian.PutUint32(unknown[installBuildPIDOffset:installBuildPIDOffset+4], 0)
	tests := []struct {
		name string
		raw  []byte
		err  error
	}{
		{"empty", nil, nil}, {"live member", encodeInstallBuildMember(live), nil}, {"missing leader", encodeInstallBuildMember(missing), nil},
		{"wrong parent", encodeInstallBuildMember(wrongParent), nil}, {"wrong uid", encodeInstallBuildMember(wrongUID), nil}, {"wrong group", encodeInstallBuildMember(wrongGroup), nil}, {"wrong start", wrongStart, nil},
		{"unknown member", unknown, nil}, {"truncated", raw[:len(raw)-1], nil}, {"duplicate", append(append([]byte(nil), raw...), raw...), nil},
		{"query failed", raw, unix.EIO}, {"query incomplete", raw, unix.ENOMEM},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if installBuildZombieGroup(ref, test.raw, test.err) {
				t.Fatal("BUILD_ZOMBIE_GROUP_PROOF_ACCEPTED_UNOWNED_MEMBERS", test.name)
			}
		})
	}
}

func encodeInstallBuildMember(member installBuildGroupMember) []byte {
	raw := make([]byte, unix.SizeofKinfoProc)
	write := func(offset uintptr, value int) { binary.NativeEndian.PutUint32(raw[offset:offset+4], uint32(value)) }
	write(installBuildPIDOffset, member.pid)
	write(installBuildParentOffset, member.parent)
	write(installBuildGroupOffset, member.group)
	binary.NativeEndian.PutUint32(raw[installBuildUIDOffset:installBuildUIDOffset+4], member.uid)
	raw[installBuildStateOffset] = member.state
	seconds, micros, _ := strings.Cut(member.start, ".")
	sec, _ := strconv.ParseUint(seconds, 10, 64)
	usec, _ := strconv.ParseUint(micros, 10, 32)
	binary.NativeEndian.PutUint64(raw[installBuildSecondsOffset:installBuildSecondsOffset+8], sec)
	binary.NativeEndian.PutUint32(raw[installBuildMicrosOffset:installBuildMicrosOffset+4], uint32(usec))
	return raw
}

func TestInstallBuildGroupEPERMRequiresZombieProof(t *testing.T) {
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sh", "-c", "sleep 0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	info, err := procwatch.Read(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ref := installBuildChildRef{identity: procwatch.Identity{PID: info.PID, Start: info.Start}, parent: info.PPID, uid: info.UID}
	leader := installBuildGroupMember{pid: info.PID, parent: info.PPID, group: info.PID, uid: info.UID, state: 5, start: info.Start}
	priorSignal, priorQuery := signalInstallBuildGroup, readInstallBuildGroup
	signalInstallBuildGroup = func(int, syscall.Signal) error { return syscall.EPERM }
	defer func() { signalInstallBuildGroup, readInstallBuildGroup = priorSignal, priorQuery }()
	for _, test := range []struct {
		name    string
		state   byte
		missing bool
		query   error
		accept  bool
	}{
		{"zombie", 5, false, nil, true}, {"live", 2, false, nil, false}, {"missing leader", 5, true, nil, false}, {"query failure", 5, false, unix.EIO, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			member := leader
			member.state = test.state
			if test.missing {
				member.pid++
			}
			readInstallBuildGroup = func(string, ...int) ([]byte, error) { return encodeInstallBuildMember(member), test.query }
			err := stopInstallBuildChild(cmd, ref)
			if test.accept && err != nil || !test.accept && !errors.Is(err, syscall.EPERM) {
				t.Fatal("BUILD_EPERM_PROOF_LOST_OR_OVERBROAD", err)
			}
		})
	}
}
