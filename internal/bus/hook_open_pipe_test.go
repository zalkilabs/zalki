package bus

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestHookPayloadReturnsWithStdinOpen(t *testing.T) {
	for _, test := range []struct {
		name        string
		size, chunk int
		delay       time.Duration
	}{
		{"small", 500, 1024, 0}, {"large", 131000, 256 * 1024, 0}, {"slow_chunks", 500, 17, time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := fixtureStore(t)
			target, _ := register(t, store, "claude")
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			written := make(chan error, 1)
			defer func() { _ = input.Close(); _ = writer.Close(); <-written }()
			done := make(chan error, 1)
			go func() { done <- (&Service{Store: store}).Hook(t.Context(), "claude", input, io.Discard) }()
			go func() {
				writeErr := writeHookChunks(writer, hookPayload(target.NativeID, "PostToolUse", test.size), test.chunk, test.delay)
				written <- writeErr
			}()
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				_ = input.Close()
				<-done
				t.Fatal("hook waited for EOF after a complete first value")
			}
			current, err := store.session(target.ID)
			if err != nil || current.State != model.StateBusy {
				t.Fatal("held-open event not applied", err)
			}
		})
	}
}

func writeHookChunks(output io.Writer, payload string, chunk int, delay time.Duration) error {
	for len(payload) > 0 {
		n, err := io.WriteString(output, payload[:min(chunk, len(payload))])
		if err != nil {
			return err
		}
		payload = payload[n:]
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	return nil
}
