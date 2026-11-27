//go:build darwin || linux

package main

import (
	"io"
	"os"
)

// writerIsTerminal: only a real file that is a terminal. A pipe, a file and a
// test buffer are not.
func writerIsTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && isInstallTerminal(file)
}
