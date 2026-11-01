package cursor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
)

type fakeLease struct {
	generation string
	pending    map[string]bool // message ID -> acknowledged
	validated  []string
	emitted    []string
}

func (f *fakeLease) Generation() string { return f.generation }
func (f *fakeLease) ValidateDelivery(id string) (bool, error) {
	f.validated = append(f.validated, id)
	acknowledged, ok := f.pending[id]
	if !ok {
		return false, errors.New("not a pending message")
	}
	return acknowledged, nil
}
func (f *fakeLease) WakeEmitted(id string) { f.emitted = append(f.emitted, id) }

type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// sendHint delivers one hint to consumeHint and returns whatever was echoed to
// the sender, and consumeHint's error.
func sendHint(t *testing.T, lease *fakeLease, hint wakeHint, output io.Writer) ([]byte, error) {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- consumeHint(server, lease, output)
	}()
	if err := json.NewEncoder(client).Encode(hint); err != nil {
		t.Fatal(err)
	}
	echo, _ := io.ReadAll(client)
	return echo, <-done
}

func TestCursorHintRejectsMisboundHintsWithoutOutput(t *testing.T) {
	pending := newTestID()
	for name, hint := range map[string]wakeHint{
		"stale generation":  {MessageID: pending, Generation: "old-generation"},
		"unknown message":   {MessageID: newTestID(), Generation: "current"},
		"injected identity": {MessageID: "\nZALKI_WAKE injected", Generation: "current"},
	} {
		t.Run(name, func(t *testing.T) {
			lease := &fakeLease{generation: "current", pending: map[string]bool{pending: false}}
			var out bytes.Buffer
			echo, err := sendHint(t, lease, hint, &out)
			if err == nil || out.Len() != 0 || len(echo) != 0 || len(lease.emitted) != 0 {
				t.Fatalf("misbound hint produced output or echo: out=%q echo=%q err=%v", out.String(), echo, err)
			}
			if name == "stale generation" && len(lease.validated) != 0 {
				t.Fatal("a stale generation reached journal validation")
			}
		})
	}
}

func TestCursorHintEmitsOnlyUnacknowledgedMessages(t *testing.T) {
	fresh, acked := newTestID(), newTestID()
	lease := &fakeLease{generation: "current", pending: map[string]bool{fresh: false, acked: true}}
	var out bytes.Buffer
	echo, err := sendHint(t, lease, wakeHint{MessageID: fresh, Generation: "current"}, &out)
	if err != nil || out.String() != "ZALKI_WAKE "+fresh+"\n" || len(lease.emitted) != 1 {
		t.Fatalf("valid hint did not wake the model once: %q %v", out.String(), err)
	}
	var response wakeHint
	if json.Unmarshal(echo, &response) != nil || response != (wakeHint{MessageID: fresh, Generation: "current"}) {
		t.Fatalf("valid hint was not echoed exactly: %q", echo)
	}
	out.Reset()
	echo, err = sendHint(t, lease, wakeHint{MessageID: acked, Generation: "current"}, &out)
	if err != nil || out.Len() != 0 || len(echo) == 0 || len(lease.emitted) != 1 {
		t.Fatalf("acknowledged message woke the model again or lost its echo: %q %q %v", out.String(), echo, err)
	}
}

func TestCursorWakeOutputFailurePreservesCause(t *testing.T) {
	id := newTestID()
	lease := &fakeLease{generation: "current", pending: map[string]bool{id: false}}
	echo, err := sendHint(t, lease, wakeHint{MessageID: id, Generation: "current"}, failingOutput{})
	if !errors.Is(err, errListenerOutput) || !errors.Is(err, io.ErrClosedPipe) || len(echo) != 0 {
		t.Fatalf("wake output failure lost its classification or underlying cause: %v", err)
	}
}
