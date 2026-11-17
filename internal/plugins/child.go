package plugins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

var errChildCanceled = errors.New("plugin canceled pipe I/O")

type child struct {
	cmd             *exec.Cmd
	input           *os.File
	output          *os.File
	reader          *bufio.Reader
	done            chan struct{}
	waitErr         error
	stderr          []byte
	overflow        bool
	cancel          context.CancelFunc
	parent          context.Context
	killedForCancel bool
	stopOnce        sync.Once
	readMode        atomic.Bool
}

func (c *child) Write(p []byte) (int, error) {
	remaining := 8192 - len(c.stderr)
	c.stderr = append(c.stderr, p[:min(remaining, len(p))]...)
	if len(p) > remaining {
		c.overflow = true
		c.cancel()
	}
	return len(p), nil
}

// launchChild starts binary, which the caller verified while it had the
// identity pinned. If the started file is no longer that file, the child is
// killed and reaped and the start fails: no retry and no new pin.
func launchChild(ctx context.Context, binary string, pinned fileIdentity, args []string, cwd, home string, lease *os.File, ownGroup bool) (*child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	c := &child{done: make(chan struct{}), cancel: cancel, parent: parent}
	input, parentInput, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	defer input.Close() //nolint:errcheck // Parent closes its copy after Start duplicates the descriptor.
	c.input = parentInput
	parentOutput, output, err := os.Pipe()
	if err != nil {
		cancel()
		_ = parentInput.Close()
		return nil, err
	}
	defer output.Close() //nolint:errcheck // Parent closes its copy after Start duplicates the descriptor.
	c.output, c.reader = parentOutput, bufio.NewReaderSize(parentOutput, maxFrame+1)
	if home == "" {
		home, err = os.UserHomeDir()
	}
	if err == nil {
		err = cleanHome(home)
	}
	if err == nil {
		// stop owns cancellation; the exec watcher must not preempt EOF grace.
		c.cmd = exec.CommandContext(context.WithoutCancel(ctx), binary, args...)
		c.cmd.Dir = cwd
		// A supervised plugin stays in its host's group, which the supervisor
		// cleans as a whole; a manual one gets its own group, as before.
		c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: ownGroup}
		c.cmd.Env = childEnv(home)
		c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = input, output, c
		c.cmd.ExtraFiles, err = isolatedChildFiles(lease)
		c.cmd.WaitDelay = time.Second
		if err == nil {
			err = c.cmd.Start()
		}
	}
	if err != nil {
		cancel()
		_ = parentInput.Close()
		_ = parentOutput.Close()
		return nil, err
	}
	go func() {
		c.waitErr = c.cmd.Wait()
		close(c.done)
	}()
	context.AfterFunc(ctx, func() { _ = c.stop(nil) })
	if !stillPinned(binary, pinned) {
		_ = c.cmd.Process.Kill()
		_ = c.stop(nil)
		return nil, errPinChangedAtStart
	}
	return c, nil
}

// Unix exec preserves unrelated inherited descriptors without CLOEXEC.
// Nil ExtraFiles entries close those slots in the forked child; they never
// close or change flags on the running parent's descriptors. The explicit
// lease at FD 3 remains the only allowed extra file.
const maxInheritedPluginFD = 1<<20 - 1 // At most 8 MiB of nil slots on 64-bit hosts.

var childFDInventory = readChildFDNames

// ReadDir may stat DT_UNKNOWN entries. Darwin's descriptor filesystem does
// not support all such per-entry metadata queries; only the names are needed.
func readChildFDNames() ([]string, error) {
	directory, err := os.Open("/dev/fd")
	if err != nil {
		return nil, err
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	return names, nil
}

func isolatedChildFiles(lease *os.File) ([]*os.File, error) {
	entries, err := childFDInventory()
	if err != nil {
		return nil, fmt.Errorf("cannot enumerate inherited plugin descriptors: %w", err)
	}
	highest := 3
	for _, name := range entries {
		fd, err := strconv.Atoi(name)
		if err != nil || fd < 0 {
			return nil, errors.New("invalid inherited plugin descriptor inventory")
		}
		if fd > maxInheritedPluginFD {
			return nil, errors.New("inherited plugin descriptor inventory exceeds isolated launch bound")
		}
		highest = max(highest, fd)
	}
	files := make([]*os.File, highest-2)
	files[0] = lease
	return files, nil
}

func (c *child) receive(timeout time.Duration) (frame, error) {
	if err := c.output.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return frame{}, c.deadlineError(c.output, err)
	}
	value, err := readFrame(c.reader)
	return value, c.pipeError(err)
}

func (c *child) idle() error {
	for {
		f, err := c.receive(250 * time.Millisecond)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("plugin stream failed: %w", err)
		}
		if !f.isRequest() {
			return errors.New("plugin sent an unsolicited frame")
		}
		if err := c.answerCheckRequest(f); err != nil {
			return err
		}
	}
}

// answerCheckRequest answers a request during check, where nothing is
// stored: inbox_next finds an empty inbox, every other op is unavailable.
func (c *child) answerCheckRequest(f frame) error {
	cmd, ok := parseCommand(f)
	if !ok || cmd.Ack != "" {
		return errors.New("plugin sent an invalid request")
	}
	code := "unavailable"
	if cmd.Op == "inbox_next" {
		code = "ok"
	}
	if err := c.input.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return c.deadlineError(c.input, err)
	}
	return c.pipeError(writeFrame(c.input, v2Reply{Reply: &v2Result{Op: cmd.Op, RequestID: cmd.RequestID, Code: code}}))
}

func (c *child) exchange(value frame) error {
	if err := c.input.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return c.deadlineError(c.input, err)
	}
	if err := writeFrame(c.input, value); err != nil {
		return c.pipeError(err)
	}
	response, err := c.receive(5 * time.Second)
	for err == nil && response.isRequest() {
		if err = c.answerCheckRequest(response); err == nil {
			response, err = c.receive(5 * time.Second)
		}
	}
	if err != nil {
		return err
	}
	if response.Ack != value.ID || response.ID != "" || response.Gap != "" || response.Event != nil || response.Hello != nil {
		return errors.New("plugin returned an invalid ACK")
	}
	return nil
}

func (c *child) stop(cause error) error {
	c.stopOnce.Do(func() {
		_ = c.input.Close()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			if err := c.cmd.Process.Kill(); err == nil {
				c.killedForCancel = c.parent.Err() != nil
			}
			<-c.done
		}
		c.cancel()
		_ = c.output.Close()
	})
	if c.overflow {
		return errors.New("plugin stderr exceeded 8KiB")
	}
	cause = errors.Join(cause, c.waitErr)
	if cause == nil {
		return nil
	}
	// A host-made hint (old wire, missing capability, a refused operation)
	// is the owner's fix, shown alone: never with child stderr.
	var hint hostHintError
	if errors.As(cause, &hint) {
		return hint
	}
	if c.readMode.Load() {
		// Its stderr may hold message text, so it is not shown.
		return errors.New("plugin request host failed")
	}
	return fmt.Errorf("%w; child stderr: %s", cause, plainStderr(c.stderr))
}

func (c *child) deadlineError(file *os.File, err error) error {
	if c.parent.Err() != nil {
		if _, statErr := file.Stat(); errors.Is(statErr, os.ErrClosed) {
			return errors.Join(errChildCanceled, err)
		}
	}
	return c.pipeError(err)
}

func (c *child) pipeError(err error) error {
	if c.parent.Err() != nil && (errors.Is(err, io.EOF) || errors.Is(err, errPartialFrameEOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrDeadlineExceeded)) {
		return errors.Join(errChildCanceled, err)
	}
	return err
}

// canceledStop is called only after stop has joined Wait and stderr copying.
func (c *child) canceledStop(ctx context.Context, cause error) bool {
	if ctx.Err() == nil || c.overflow {
		return false
	}
	if cause != nil && !errors.Is(cause, ctx.Err()) && !errors.Is(cause, errChildCanceled) {
		return false
	}
	if c.waitErr == nil || errors.Is(c.waitErr, ctx.Err()) {
		return true
	}
	var exit *exec.ExitError
	if !errors.As(c.waitErr, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return c.killedForCancel && ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// finish stops and reaps the child at the end of Run. requested: the host
// stopped it for the owner's disable, so any exit code of a child that left
// by itself is a quiet stop; a kill after the grace stays an error, also when
// the run is canceled meanwhile. Otherwise the cause stays, unless the stop
// was the run's own cancellation.
func (c *child) finish(ctx context.Context, cause error, requested bool) error {
	if requested {
		err := c.stop(nil)
		if c.requestedStopClean() {
			return nil
		}
		return err
	}
	err := c.stop(cause)
	if c.canceledStop(ctx, cause) {
		return nil
	}
	return err
}

// requestedStopClean is called only after stop has joined Wait: the child
// left by itself (any exit code) and its stderr stayed in bounds. A signal
// death, including the kill after the grace, is not clean.
func (c *child) requestedStopClean() bool {
	if c.overflow {
		return false
	}
	if c.waitErr == nil {
		return true
	}
	var exit *exec.ExitError
	if !errors.As(c.waitErr, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Exited()
}

// plainStderr keeps line breaks, tabs and printable text of plugin stderr. Any
// other rune (ESC, BEL, bidi controls) or invalid byte is shown as a Go escape,
// so the plugin cannot drive the owner's terminal through an error message.
func plainStderr(b []byte) string {
	var out strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&out, `\x%02x`, b[0])
		case r == '\n' || r == '\t' || unicode.IsPrint(r):
			out.Write(b[:size])
		default:
			quoted := strconv.QuoteRune(r)
			out.WriteString(quoted[1 : len(quoted)-1])
		}
		b = b[size:]
	}
	return out.String()
}
