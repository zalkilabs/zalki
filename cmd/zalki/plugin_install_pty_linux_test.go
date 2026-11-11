package main

import (
	"os"
	"strconv"
	"syscall"

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
	unlock := 0
	err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, unlock)
	n := 0
	if err == nil {
		n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN)
	}
	if err != nil {
		_ = master.Close()
		return nil, "", err
	}
	return master, "/dev/pts/" + strconv.Itoa(n), nil
}
