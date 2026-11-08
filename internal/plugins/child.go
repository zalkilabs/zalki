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
	"syscall"
	"time"
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
	home, err := os.UserHomeDir()
	if err == nil {
		c.cmd = exec.CommandContext(ctx, binary, ChildCommand, "--outbox", outbox)
		c.cmd.Cancel = func() error {
			err := c.cmd.Process.Kill()
			c.killedForCancel = err == nil
			return err
		}
		c.cmd.Dir = filepath.Dir(outbox)
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
	return c, nil
}

func (c *child) receive(timeout time.Duration) (frame, error) {
	if err := c.output.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return frame{}, c.pipeError(err)
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
		return c.pipeError(err)
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
	_ = c.input.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
	c.cancel()
	_ = c.output.Close()
	if c.overflow {
		return errors.New("plugin stderr exceeded 8KiB")
	}
	cause = errors.Join(cause, c.waitErr)
	if cause == nil {
		return nil
	}
	return fmt.Errorf("%w; child stderr: %s", cause, c.stderr)
}

func (c *child) pipeError(err error) error {
	if c.parent.Err() != nil && (errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE)) {
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
