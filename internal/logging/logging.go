// Package logging configures process-local structured diagnostics. It never
// changes slog's global default or writes to the protocol's stdout.
package logging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Config struct {
	Level            string
	File             string // Absolute path, or "-" for stderr only.
	ExistingStateDir string // Default sink starts only after the journal exists.
}

func New(config Config, stderr io.Writer) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(config.Level) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "off":
		level = 100
	default:
		return nil, errors.New("log level must be debug, info, warn, error, or off")
	}
	if stderr == nil {
		stderr = io.Discard
	}
	output := stderr
	if config.File != "-" {
		if !filepath.IsAbs(config.File) {
			return nil, errors.New("log file must be an absolute path or - for stderr")
		}
		output = &fallbackWriter{
			primary:  &rotatingWriter{path: config.File, stateRoot: config.ExistingStateDir, maxBytes: 5 * 1024 * 1024, backups: 3},
			fallback: stderr,
		}
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})), nil
}

// A diagnostic sink failure must not change a committed message's result.
// Do not include the underlying error: paths and external errors can contain
// credentials or peer text. The original sanitized event is kept on stderr.
type fallbackWriter struct {
	mu                sync.Mutex
	primary, fallback io.Writer
	reported          bool
}

func (w *fallbackWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n, err := w.primary.Write(p); err == nil && n == len(p) {
		return n, nil
	}
	if !w.reported {
		w.reported = true
		slog.New(slog.NewJSONHandler(w.fallback, nil)).Warn("logging.fallback", "app", "zalki", "pid", os.Getpid(), "error_code", "log_sink_unavailable")
	}
	_, _ = w.fallback.Write(p)
	return len(p), nil
}
