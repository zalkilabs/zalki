package main

import (
	"errors"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

var unlinkAliasDirectory = removeAliasDirectory

// The opened parent anchors lookup; AT_REMOVEDIR refuses a final replacement
// file or symlink even if it arrived after the caller's identity check.
func removeAliasDirectory(parent *os.Root, name string) (err error) {
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	fd := dir.Fd()
	if fd > uintptr(math.MaxInt) {
		return errors.New("invalid directory descriptor")
	}
	return unix.Unlinkat(int(fd), name, unix.AT_REMOVEDIR)
}
