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
		os.Exit(1)
	}
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
