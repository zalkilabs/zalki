package plugins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func startChild(ctx context.Context, binary, outbox string, lease *os.File) (*child, error) {
	return launchChild(ctx, binary, []string{ChildCommand, "--outbox", outbox}, filepath.Dir(outbox), "", lease)
}

func launchChild(ctx context.Context, binary string, args []string, cwd, home string, lease *os.File) (*child, error) {
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
		// stop owns cancellation; the exec watcher must not preempt EOF grace.
		c.cmd = exec.CommandContext(context.WithoutCancel(ctx), binary, args...)
		c.cmd.Dir = cwd
		c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		c.cmd.Env = []string{"HOME=" + home}
		c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = input, output, c
		c.cmd.ExtraFiles = []*os.File{lease}
		c.cmd.WaitDelay = time.Second
		err = c.cmd.Start()
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
	return c, nil
}

func (c *child) receive(timeout time.Duration) (frame, error) {
	if err := c.output.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return frame{}, c.deadlineError(c.output, err)
	}
	value, err := readFrame(c.reader)
	return value, c.pipeError(err)
}

func (c *child) idle() error {
	_, err := c.receive(250 * time.Millisecond)
	if err == nil {
		return errors.New("plugin sent an unsolicited frame")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("plugin stream failed: %w", err)
	}
	return nil
}

func (c *child) exchange(value frame) error {
	if err := c.input.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return c.deadlineError(c.input, err)
	}
	if err := writeFrame(c.input, value); err != nil {
		return c.pipeError(err)
	}
	response, err := c.receive(5 * time.Second)
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
	if c.readMode.Load() {
		return errors.New("plugin v1-read failed")
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
