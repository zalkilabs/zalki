package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// logFileLimit caps each log file; with one .1 file a stream keeps at most
// twice this.
const logFileLimit = 1 << 20

// rotatingLog is a private, capped log for one stream. The controller owns
// it and copies what the host prints from a pipe into it (copyFrom), so a
// long-lived child can never keep writing a renamed file past the cap.
type rotatingLog struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	size   int64
	failed error
}

// openRotatingLog opens path for appending, refusing a link, a special
// file, another user's file or one others can read or write.
func openRotatingLog(path string) (*rotatingLog, error) {
	l := &rotatingLog{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rotatingLog) open() error {
	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return unsafeLog(l.path, err)
	}
	info, err := file.Stat()
	if err != nil || !privateFile(info) || info.Size() > logFileLimit {
		// An oversize file is refused, never moved into an oversize .1.
		_ = file.Close()
		return unsafeLog(l.path, err)
	}
	l.file, l.size = file, info.Size()
	return nil
}

func unsafeLog(path string, cause error) error {
	return errors.Join(model.NewError(model.CodeConflict, "%s is not a private log file of this user; it was kept", path), cause)
}

// write appends p, rotating at the cap. A write is split so no file grows
// past the cap. After a failure every later write fails: the controller
// stops the host rather than lose the cap or print elsewhere.
func (l *rotatingLog) write(p []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	written := 0
	for l.failed == nil && written < len(p) {
		if l.size >= logFileLimit {
			l.failed = l.rotate()
			continue
		}
		n, err := l.file.Write(p[written:min(len(p), written+int(logFileLimit-l.size))])
		written, l.size, l.failed = written+n, l.size+int64(n), err
	}
	return l.failed
}

// rotate keeps exactly one older file. An existing .1 must itself be a
// private file of this user before it is replaced.
func (l *rotatingLog) rotate() error {
	// The path must still be the file we write: a replaced leaf is kept.
	opened, err := l.file.Stat()
	if err != nil {
		return err
	}
	if current, err := os.Lstat(l.path); err != nil || !os.SameFile(opened, current) {
		return unsafeLog(l.path, err)
	}
	older := l.path + ".1"
	if info, err := os.Lstat(older); err == nil && !privateFile(info) {
		return unsafeLog(older, nil)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := errors.Join(l.file.Sync(), l.file.Close()); err != nil {
		return err
	}
	if err := os.Rename(l.path, older); err != nil {
		return err
	}
	return l.open()
}

// failure reports a failed log; the controller stops that host.
func (l *rotatingLog) failure() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failed
}

func (l *rotatingLog) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

// pluginLogHelp is the owner-facing note on what the log may hold.
const pluginLogHelp = "The per-plugin log (0600) may contain whatever the plugin prints, including secrets. plugins list/status never prints log content."

const logTailLines = 50

func newPluginLogsCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "logs NAME", Short: "Show the last 50 lines of a plugin's private log", Long: pluginLogHelp, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if len(name) > 32 || !model.ValidKind(name) {
				return model.NewError(model.CodeInvalidInput, "plugin name must match [a-z][a-z0-9-]{0,31}")
			}
			service, err := opts.service()
			if err != nil {
				return err
			}
			lines, err := tailPluginLog(filepath.Join(service.Store.Dir, "plugins", name+".log"))
			if err != nil {
				return err
			}
			if len(lines) == 0 {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "No log for %s yet.\n", name)
				return err
			}
			for _, line := range lines {
				// Escape and bidi characters or invalid bytes cannot drive the terminal.
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), plainText(strings.ToValidUTF8(line, "\uFFFD"))); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// tailPluginLog returns the last lines of the older and the active file,
// reading at most the two capped files from private files of this user.
func tailPluginLog(path string) ([]string, error) {
	var text []byte
	for _, file := range []string{path + ".1", path} {
		info, err := os.Lstat(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !privateFile(info) {
			return nil, unsafeLog(file, err)
		}
		raw, err := journal.ReadBytes(file, logFileLimit)
		if err != nil {
			return nil, unsafeLog(file, err)
		}
		text = append(text, raw...)
	}
	lines := strings.Split(strings.TrimRight(string(text), "\n"), "\n")
	if len(text) == 0 {
		return nil, nil
	}
	return lines[max(0, len(lines)-logTailLines):], nil
}

// copyFrom copies the host's output into the log until the pipe ends or the
// log fails; after a failure the rest is drained, so the host never blocks.
func (l *rotatingLog) copyFrom(source io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := source.Read(buf)
		if n > 0 && l.failure() == nil {
			_ = l.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}
