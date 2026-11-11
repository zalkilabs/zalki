package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openTestPTY opens a pseudo-terminal pair: the master for the test, the
// slave path for the child's controlling terminal.
func openTestPTY() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	fd := int(master.Fd())
	var name [128]byte
	err = unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0)
	if err == nil {
		err = unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0)
	}
	if err == nil {
		// x/sys has no darwin wrapper for TIOCPTYGNAME; the standard syscall
		// package still offers the plain ioctl.
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
			err = errno
		}
	}
	if err != nil {
		_ = master.Close()
		return nil, "", err
	}
	return master, strings.TrimRight(string(name[:]), "\x00"), nil
}
