package adapters

import (
	"context"
	"net"
	"time"
)

// bindSocketContext preserves the transport's I/O bound while honoring earlier
// caller deadlines and cancellation after DialContext has returned. Cleanup joins
// any running cancellation callback before the caller releases the connection.
func bindSocketContext(ctx context.Context, conn net.Conn, limit time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(limit)
	if caller, ok := ctx.Deadline(); ok && caller.Before(deadline) {
		deadline = caller
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}, nil
}
