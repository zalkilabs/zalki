package main

import "golang.org/x/sys/unix"

const termiosGet, termiosSet = unix.TIOCGETA, unix.TIOCSETA
