package main

import (
	"context"
	"errors"
	"io"
	"os/signal"
)

// errInstallCancelled marks an install the owner stopped. It is reported as
// "Cancelled".
//
// Ctrl-C reaches the install by one of two routes:
//   - raw keys (checkbox screen): the terminal sends byte 3 as input and no
//     signal; readInstallKey turns it into the "ctrl-c" key, which means No.
//   - plain lines (and the build, commit and other output after the screen):
//     the terminal sends SIGINT, main's signal.NotifyContext cancels the
//     command context, and contextReader stops waiting for input.
//
// SIGTERM and SIGHUP always take the second route.
var errInstallCancelled = errors.New("install cancelled")

// contextReader stops waiting for input when the context ends. In line mode
// the terminal turns Ctrl-C into SIGINT, which only cancels the context, so a
// plain read would keep waiting and the questions would go on.
//
// Single use: the command must end after a cancel. A read that is waiting when
// the cancel comes stays in its goroutine (a blocking terminal read cannot be
// stopped safely), so the install prints "Cancelled", returns to main and the
// process exits; the goroutine ends with it and never takes later input.
type contextReader struct {
	ctx     context.Context
	r       io.Reader
	results chan contextRead
	pending bool
	left    []byte
}

type contextRead struct {
	data []byte
	err  error
}

func newContextReader(ctx context.Context, r io.Reader) *contextReader {
	return &contextReader{ctx: ctx, r: r, results: make(chan contextRead, 1)}
}

func (c *contextReader) Read(p []byte) (int, error) {
	if len(c.left) > 0 {
		n := copy(p, c.left)
		c.left = c.left[n:]
		return n, nil
	}
	if err := c.ctx.Err(); err != nil {
		return 0, err // after a cancel no new read starts
	}
	if !c.pending {
		c.pending = true
		size := len(p)
		go func() {
			buf := make([]byte, size)
			n, err := c.r.Read(buf)
			c.results <- contextRead{buf[:n], err}
		}()
	}
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	case got := <-c.results:
		c.pending = false
		if err := c.ctx.Err(); err != nil {
			return 0, err // input that arrives with the cancel is not used
		}
		n := copy(p, got.data)
		c.left = got.data[n:]
		return n, got.err
	}
}

// restoreSignalsOnCancel gives the signals their default action back at the
// first cancel, so a second Ctrl-C ends the process at once, even while the
// install waits at its point of no return. The returned func stops watching.
func restoreSignalsOnCancel(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			signal.Reset(cancelSignals...)
		case <-done:
		}
	}()
	return func() { close(done) }
}

// installStep runs before the build and before the commit; tests use it to
// cancel at an exact point.
var installStep = func(string) {}

// cancelled reports whether err comes from the owner stopping the command.
func cancelled(err error) bool {
	return errors.Is(err, errInstallCancelled) || errors.Is(err, context.Canceled)
}
