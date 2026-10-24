package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
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
