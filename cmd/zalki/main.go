package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// cancelSignals cancel the command context. plugins install gives them their
// default action back at the first cancel (restoreSignalsOnCancel).
var cancelSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), cancelSignals...)
	defer cancel()
	ctx = context.WithValue(ctx, signalContextKey{}, ctx)
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "zalki:", err)
		os.Exit(commandExitCode(err))
	}
}

// supervisorExitCode: the supervisor's two special outcomes, matched by
// direct identity (a joined or wrapped error is an ordinary failure): 75
// asks launchd for a new controller, 0 is a permanent setup error a
// restart cannot help. ok is false for every other error.
func supervisorExitCode(err error) (code int, ok bool) {
	if err == errZalkiReplaced { //nolint:errorlint // Direct identity on purpose: a joined or wrapped error is an ordinary failure.
		return exitReplaced, true
	}
	if err == errUnsafePluginsDir { //nolint:errorlint // Direct identity on purpose, as above.
		return 0, true
	}
	return 0, false
}

func run(ctx context.Context, argv []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cmd := newRootCommand(home)
	cmd.SetIn(os.Stdin)
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	cmd.SetArgs(argv)
	return cmd.ExecuteContext(ctx)
}
