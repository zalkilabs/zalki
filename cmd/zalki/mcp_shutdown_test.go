package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func markedSignalContext(t *testing.T) (context.Context, context.CancelCauseFunc) {
	t.Helper()
	signalCtx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return context.WithValue(signalCtx, signalContextKey{}, signalCtx), cancel
}

func TestMCPStopErrorCleansOnlyCanonicalCancellation(t *testing.T) {
	cause := errors.New("test OS signal cause")
	ctx, cancel := markedSignalContext(t)
	cancel(cause)
	ioErr := errors.New("IO error")
	wrapped := fmt.Errorf("wrapped: %w", context.Canceled)
	joined := errors.Join(context.Canceled, ioErr)
	for _, test := range []struct {
		input, want error
	}{
		{nil, nil},
		{context.Canceled, nil},
		{context.DeadlineExceeded, context.DeadlineExceeded},
		{ioErr, ioErr},
		{wrapped, wrapped},
		{joined, joined},
	} {
		if got := mcpStopError(ctx, test.input); !errors.Is(got, test.want) {
			t.Fatalf("mcpStopError for %v returned %v, want %v", test.input, got, test.want)
		}
	}
}

func TestMCPStopErrorPreservesOtherCancellationOrigins(t *testing.T) {
	signalCause := errors.New("test OS signal cause")
	for _, test := range []struct {
		name  string
		setup func(context.Context, context.CancelCauseFunc) context.Context
	}{
		{"unmarked", func(_ context.Context, _ context.CancelCauseFunc) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
		{"unmarked_custom_cause", func(_ context.Context, _ context.CancelCauseFunc) context.Context {
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(errors.New("parent reason"))
			return ctx
		}},
		{"wrong_marker", func(_ context.Context, _ context.CancelCauseFunc) context.Context {
			return context.WithValue(context.Background(), signalContextKey{}, "not a context")
		}},
		{"live_signal_context", func(ctx context.Context, _ context.CancelCauseFunc) context.Context { return ctx }},
		{"stop_function", func(ctx context.Context, cancel context.CancelCauseFunc) context.Context { cancel(nil); return ctx }},
		{"child_cancel_before_signal", func(ctx context.Context, cancel context.CancelCauseFunc) context.Context {
			child, stop := context.WithCancel(ctx)
			stop()
			cancel(signalCause)
			return child
		}},
		{"child_deadline_before_signal", func(ctx context.Context, cancel context.CancelCauseFunc) context.Context {
			child, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
			stop()
			cancel(signalCause)
			return child
		}},
		{"custom_child_cause_before_signal", func(ctx context.Context, cancel context.CancelCauseFunc) context.Context {
			child, stop := context.WithCancelCause(ctx)
			stop(errors.New("parent stop"))
			cancel(signalCause)
			return child
		}},
		{"detached_child", func(ctx context.Context, cancel context.CancelCauseFunc) context.Context {
			child := context.WithoutCancel(ctx)
			cancel(signalCause)
			return child
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := markedSignalContext(t)
			ctx = test.setup(ctx, cancel)
			if got := mcpStopError(ctx, context.Canceled); !errors.Is(got, context.Canceled) {
				t.Fatalf("independent cancellation was hidden: %v", got)
			}
		})
	}
}

func TestMCPParentCancellationAndDeadlineRemainErrors(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(strconv.FormatBool(deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			want := context.Canceled
			if deadline {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer stop()
				want = context.DeadlineExceeded
			}
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer writer.Close()
			output, sink, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			defer sink.Close()
			cmd := newRootCommand(t.TempDir())
			var diagnostic bytes.Buffer
			cmd.SetIn(input)
			cmd.SetOut(sink)
			cmd.SetErr(&diagnostic)
			cmd.SetArgs([]string{"--state", filepath.Join(t.TempDir(), "state"), "--log-file", "-", "mcp"})
			err = cmd.ExecuteContext(ctx)
			if !errors.Is(err, want) || !strings.Contains(diagnostic.String(), `"msg":"command.failed"`) || !strings.Contains(diagnostic.String(), `"level":"ERROR"`) {
				t.Fatalf("parent cancel/deadline hidden: %v %s", err, diagnostic.String())
			}
		})
	}
}

type lostHookOutput struct{ written int }

func (w *lostHookOutput) Write(data []byte) (int, error) {
	w.written++
	return min(1, len(data)), context.Canceled
}

func TestHookPartialOutputCancellationRemainsFailure(t *testing.T) {
	t.Setenv("ZALKI_CAPTURE_CONTRACTS", "")
	t.Setenv("CODEX_HOME", t.TempDir())
	ctx, cancel := markedSignalContext(t)
	cancel(errors.New("test OS signal cause"))
	home := t.TempDir()
	input, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": "00000000-0000-4000-8000-000000000104", "cwd": home})
	cmd := newRootCommand(home)
	var diagnostic bytes.Buffer
	output := &lostHookOutput{}
	cmd.SetIn(bytes.NewReader(input))
	cmd.SetOut(output)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs([]string{"--state", filepath.Join(home, "state"), "--log-file", "-", "hook", "--agent", "codex"})
	err := cmd.ExecuteContext(ctx)
	if !errors.Is(err, context.Canceled) || output.written != 1 || !strings.Contains(diagnostic.String(), `"msg":"command.failed"`) {
		t.Fatalf("lost hook write was reported clean: %v %d %s", err, output.written, diagnostic.String())
	}
}
