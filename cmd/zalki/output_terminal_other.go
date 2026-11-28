//go:build !darwin && !linux

package main

import "io"

// writerIsTerminal: no terminal detection on this system, so plain text.
func writerIsTerminal(io.Writer) bool { return false }
