package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zalkilabs/zalki/internal/bus"
	"net"
	"testing"
	"time"
)

func TestCursorSocketHonorsContext(t *testing.T) {
	for _, mode := range []string{"cancel after hint", "caller deadline", "already canceled"} {
		t.Run(mode, func(t *testing.T) {
			listener, endpoint := listenCursorSocket(t)
			state, err := json.Marshal(bus.CursorListenerState{Schema: 1, Wake: endpoint})
			if err != nil {
				t.Fatal(err)
			}
			target := bus.Session{Kind: "cursor", Generation: newTestID(), AdapterState: state}
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "caller deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
			}
			defer cancel()
			if mode == "already canceled" {
				cancel()
				receipt := (Adapter{}).Deliver(ctx, bus.Message{ID: newTestID()}, target)
				if receipt.Status != bus.StatusPendingActivity {
					t.Fatalf("unsent hint became uncertain: %+v", receipt)
				}
				assertNoSocketConnection(t, listener)
				return
			}
			result := make(chan bus.Receipt, 1)
			go func() { result <- (Adapter{}).Deliver(ctx, bus.Message{ID: newTestID()}, target) }()
			conn, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var hint bus.CursorWakeHint
			if err := json.NewDecoder(conn).Decode(&hint); err != nil {
				t.Fatal("sender did not write the hint", err)
			}
			// Keep the connection open without echoing; only context can unblock
			// the sender before its existing three-second transport timeout.
			if mode == "cancel after hint" {
				cancel()
			}
			select {
			case receipt := <-result:
				if receipt.Status != bus.StatusWakeUncertain || receipt.Error == "" {
					t.Fatalf("canceled written hint lost uncertainty: %+v", receipt)
				}
			case <-time.After(time.Second):
				t.Fatal("socket I/O ignored caller cancellation/deadline")
			}
		})
	}
}

func assertNoSocketConnection(t *testing.T, listener *net.UnixListener) {
	t.Helper()
	if err := listener.SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err == nil {
		conn.Close()
		t.Fatal("canceled delivery still connected")
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("failed to observe socket", err)
	}
}
