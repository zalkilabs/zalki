package procwatch

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func darwinProcess(p unix.KinfoProc) Info {
	return Info{PID: int(p.Proc.P_pid), PPID: int(p.Eproc.Ppid),
		Start: fmt.Sprintf("%d.%06d", p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec),
		Name:  unix.ByteSliceToString(p.Proc.P_comm[:]), UID: p.Eproc.Ucred.Uid,
		Zombie: p.Proc.P_stat == 5} // SZOMB, sys/proc.h.
}

// errUnknownProcess: the process could not be inspected and its absence is
// not proven. It matches neither os.ErrNotExist nor syscall.ESRCH, so no
// caller reads it as an exit.
var errUnknownProcess = errors.New("process state unknown")

// rowQuery and probe are replaced only by tests.
var (
	rowQuery = fixedRow
	probe    = func(pid int) error { return unix.Kill(pid, 0) } // signal 0 sends nothing
)

// fixedRow reads exactly one kinfo_proc row: the capacity is fixed before the
// read, with no size query, no allocation from a kernel size and no retry. An
// answer that is not exactly one row is EIO.
func fixedRow(pid int) (*unix.KinfoProc, error) {
	return unix.SysctlKinfoProc("kern.proc.pid", pid)
}

// Read returns a snapshot of pid with one bounded row query and at most one
// signal-0 probe. Only the probe's ESRCH proves that pid is gone.
func Read(pid int) (Info, error) {
	if pid <= 0 {
		return Info{}, errUnknownProcess // kill(0 or -n) would address a group
	}
	row, err := rowQuery(pid)
	switch {
	case err == nil && row != nil && int(row.Proc.P_pid) == pid:
		return darwinProcess(*row), nil
	case err == nil:
		return Info{}, syscall.EIO // no row, or a row of another process
	case errors.Is(err, syscall.EIO), errors.Is(err, syscall.ESRCH), errors.Is(err, syscall.ENOENT):
		// An empty answer, or an error that only looks like absence.
		if errors.Is(probe(pid), syscall.ESRCH) {
			return Info{}, syscall.ESRCH
		}
		return Info{}, errUnknownProcess
	}
	return Info{}, err
}

// Named returns non-zombie processes owned by this user with the exact name.
func Named(name string) ([]Info, error) {
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	var result []Info
	for _, row := range rows {
		p := darwinProcess(row)
		if p.Name == name && p.UID == uint32(os.Getuid()) && !p.Zombie { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
			result = append(result, p)
		}
	}
	return result, nil
}
