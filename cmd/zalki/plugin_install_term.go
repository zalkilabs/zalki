//go:build darwin || linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// rawInstallTerminal switches a terminal to raw keys and returns a restore
// function. Output processing stays on so plain newlines still work.
func rawInstallTerminal(file *os.File) (func(), error) {
	fd := int(file.Fd())
	old, err := unix.IoctlGetTermios(fd, termiosGet)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Iflag &^= unix.ICRNL | unix.IXON
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, termiosSet, &raw); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, termiosSet, old) }, nil
}

func isInstallTerminal(file *os.File) bool {
	_, err := unix.IoctlGetTermios(int(file.Fd()), termiosGet)
	return err == nil
}
