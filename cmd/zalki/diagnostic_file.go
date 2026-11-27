package main

import (
	"errors"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/statefile"
)

// diagnosticFileLimit bounds a file the doctor reads for a diagnostic.
const diagnosticFileLimit = 1 << 20

// readDiagnosticFile reads one user-visible file for a diagnostic. It returns
// the bytes, or the reason it could not: "missing_file", "unsafe_file" (a
// symbolic parent, a symbolic link, a FIFO, another non-regular file, or over
// the limit) or "invalid_data" (not UTF-8). A symbolic parent can change the
// recorded source key, so uncertain path spelling is not verified.
func readDiagnosticFile(path string) ([]byte, string) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "missing_file"
	}
	if err != nil || parent != filepath.Dir(path) {
		return nil, "unsafe_file"
	}
	raw, err := statefile.ReadBytes(path, diagnosticFileLimit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "missing_file"
	}
	if err != nil {
		return nil, "unsafe_file"
	}
	if !utf8.Valid(raw) {
		return nil, "invalid_data"
	}
	return raw, ""
}
