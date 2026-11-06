package procwatch

import (
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

// Read returns a snapshot of pid, preserving OS inspection errors.
func Read(pid int) (Info, error) {
	// The singular helper turns an empty kernel result into EIO. The slice
	// helper preserves an empty result, which is positive process-exit evidence.
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return Info{}, err
	}
	if len(rows) == 0 {
		return Info{}, syscall.ESRCH
	}
	if len(rows) != 1 || int(rows[0].Proc.P_pid) != pid {
		return Info{}, syscall.EIO
	}
	return darwinProcess(rows[0]), nil
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
