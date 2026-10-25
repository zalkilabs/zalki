package logging

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

// ErrorKind keeps diagnostics useful without serializing untrusted error text,
// which can embed native command output, peer content, paths, or credentials.
func ErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case os.IsTimeout(err):
		return "timeout"
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return "invalid_json"
	}
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return "json_type_mismatch"
	}
	var network *net.OpError
	if errors.As(err, &network) {
		return "network"
	}
	var path *os.PathError
	if errors.As(err, &path) {
		return "filesystem"
	}
	return "other"
}
