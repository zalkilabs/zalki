package plugins

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type frameTerminalError struct{ err error }

func (r frameTerminalError) Read([]byte) (int, error) { return 0, r.err }

func TestWirePreservesPartialReadFailureAndCancelCause(t *testing.T) {
	for _, cause := range []error{os.ErrDeadlineExceeded, os.ErrClosed} {
		reader := bufio.NewReaderSize(io.MultiReader(strings.NewReader("{"), frameTerminalError{cause}), maxFrame+1)
		_, err := readFrame(reader)
		if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, cause) {
			t.Fatal("partial frame lost original read failure", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		c := &child{parent: ctx}
		observed := c.pipeError(err)
		cancel()
		if errors.Is(observed, errChildCanceled) {
			t.Fatal("later cancellation relabeled observed read failure")
		}
		if !errors.Is(c.pipeError(err), errChildCanceled) {
			t.Fatal("partial canceled read lost cancellation", err)
		}
	}
}

func TestWirePartialEOFIsNotCleanEOFOrCompleteEmptyFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := &child{parent: ctx}
	_, partial := readFrame(bufio.NewReaderSize(strings.NewReader("{"), maxFrame+1))
	if !errors.Is(partial, io.ErrUnexpectedEOF) || errors.Is(partial, io.EOF) {
		t.Fatal("truncated frame became clean EOF", partial)
	}
	if !errors.Is(c.pipeError(partial), errChildCanceled) {
		t.Fatal("partial EOF during cancellation was not classified", partial)
	}
	for _, raw := range []string{"\n", "{\n"} {
		_, invalid := readFrame(bufio.NewReaderSize(strings.NewReader(raw), maxFrame+1))
		if !errors.Is(invalid, io.ErrUnexpectedEOF) || errors.Is(c.pipeError(invalid), errChildCanceled) {
			t.Fatal("complete invalid frame was hidden by cancellation", raw, invalid)
		}
	}
}

func TestWireReportsOversizeBeforePartialReadError(t *testing.T) {
	_, err := readFrame(bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", maxFrame+2)), maxFrame+1))
	if err == nil || !strings.Contains(err.Error(), "exceeds 64KiB") || errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("oversize frame reported as truncated input", err)
	}
}

func TestNotifierCancellationPreservesPartialReadFailureCause(t *testing.T) {
	for _, cause := range []error{os.ErrClosed, io.ErrClosedPipe, os.ErrDeadlineExceeded} {
		for _, canceled := range []bool{false, true} {
			ctx, cancel := context.WithCancel(t.Context())
			if canceled {
				cancel()
			}
			input := io.NopCloser(io.MultiReader(strings.NewReader("{"), frameTerminalError{cause}))
			err := runNotifierLoop(ctx, input, io.Discard, notifierTemp(t))
			cancel()
			if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, cause) {
				t.Fatal("notifier lost partial read failure cause", canceled, cause, err)
			}
		}
	}
}
