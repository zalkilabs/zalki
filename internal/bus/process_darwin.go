package bus

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func darwinProcess(p unix.KinfoProc) processInfo {
	return processInfo{PID: int(p.Proc.P_pid), PPID: int(p.Eproc.Ppid),
		Start: fmt.Sprintf("%d.%06d", p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec),
		Name:  unix.ByteSliceToString(p.Proc.P_comm[:]), UID: p.Eproc.Ucred.Uid,
		Zombie: p.Proc.P_stat == 5} // SZOMB, sys/proc.h.
}

func readProcess(pid int) (processInfo, error) {
	// The singular helper turns an empty kernel result into EIO. The slice
	// helper preserves an empty result, which is positive process-exit evidence.
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return processInfo{}, err
	}
	if len(rows) == 0 {
		return processInfo{}, syscall.ESRCH
	}
	if len(rows) != 1 || int(rows[0].Proc.P_pid) != pid {
		return processInfo{}, syscall.EIO
	}
	return darwinProcess(rows[0]), nil
}

func namedProcesses(name string) ([]processInfo, error) {
	rows, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	var result []processInfo
	for _, row := range rows {
		p := darwinProcess(row)
		if p.Name == name && p.UID == uint32(os.Getuid()) && !p.Zombie {
			result = append(result, p)
		}
	}
	return result, nil
}
