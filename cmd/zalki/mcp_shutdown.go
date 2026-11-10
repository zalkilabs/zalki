package main

import "context"

// main marks its exact NotifyContext, whose parent is Background. A signal
// gives it a distinct Cause; stop() gives it context.Canceled instead.
type signalContextKey struct{}

func mcpStopError(ctx context.Context, err error) error {
	if err != context.Canceled { //nolint:errorlint // The SDK returns ctx.Err() directly. Wrapped and joined errors must be preserved.
		return err
	}
	signalCtx, ok := ctx.Value(signalContextKey{}).(context.Context)
	if !ok || signalCtx.Err() != context.Canceled {
		return err
	}
	cause := context.Cause(signalCtx)
	// A child may have canceled independently before the signal arrived.
	if cause != context.Canceled && context.Cause(ctx) == cause { //nolint:errorlint // Match the original cause by identity.
		return nil
	}
	return err
}
