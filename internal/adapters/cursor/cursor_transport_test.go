package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestCursorSenderRequiresExactHintEcho(t *testing.T) {
	for _, test := range []struct {
		name   string
		status bus.DeliveryStatus
	}{
		{"matching", bus.StatusWakeWritten},
		{"wrong generation", bus.StatusWakeUncertain},
		{"wrong message", bus.StatusWakeUncertain},
		{"missing echo", bus.StatusWakeUncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "zalki-cursor-test-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "inbox.sock"), Net: "unix"})
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				t.Skip("execution sandbox blocks Unix socket bind")
			}
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			endpoint, err := bus.InspectEndpoint(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			state, err := json.Marshal(bus.CursorListenerState{Schema: 1, Wake: endpoint})
			if err != nil {
				t.Fatal(err)
			}
			target := bus.Session{Kind: "cursor", Generation: newTestID(), AdapterState: state}
			message := bus.Message{ID: newTestID(), Body: "peer body must stay in the journal"}
			done := make(chan error, 1)
			go func() {
				conn, err := listener.AcceptUnix()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					done <- err
					return
				}
				// Reject any extra field: native wake frames must contain no body,
				// sender metadata, capability or other model-facing text.
				var hint bus.CursorWakeHint
				decoder := json.NewDecoder(conn)
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&hint); err != nil {
					done <- err
					return
				}
				if hint.MessageID != message.ID || hint.Generation != target.Generation {
					done <- errors.New("sender changed the message or registration identity")
					return
				}
				switch test.name {
				case "wrong generation":
					hint.Generation = "stale"
				case "wrong message":
					hint.MessageID = newTestID()
				case "missing echo":
					done <- nil
					return
				}
				done <- json.NewEncoder(conn).Encode(hint)
			}()
			receipt := (Adapter{}).Deliver(context.Background(), message, target)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if receipt.Status != test.status || receipt.At == "" || (receipt.Error != "") != (test.status == bus.StatusWakeUncertain) {
				t.Fatalf("sender confused wake acceptance with an uncertain echo: %+v", receipt)
			}
		})
	}
}
