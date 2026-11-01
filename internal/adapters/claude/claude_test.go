package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

func listenClaudeSocket(t *testing.T) (*net.UnixListener, *bus.Endpoint) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zalki-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "receiver.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if errors.Is(err, syscall.EPERM) {
		t.Skip("execution sandbox blocks Unix socket bind")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	endpoint, err := bus.InspectEndpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return listener, endpoint
}

func TestRealClaudeSocketFrame(t *testing.T) {
	listener, endpoint := listenClaudeSocket(t)
	msg := bus.Message{ID: newTestID(), From: "codex:" + newTestID(), FromLabel: "Codex", To: "claude:" + newTestID(), Body: "exact body"}
	receipt := writeClaude(context.Background(), msg, bus.Session{Kind: "claude", Endpoint: endpoint})
	if receipt.Status != "transport_written" {
		t.Fatalf("%+v", receipt)
	}
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var frame map[string]any
	if err := json.NewDecoder(connection).Decode(&frame); err != nil {
		t.Fatal(err)
	}
	contract := loadClaudeContract(t)
	for key, expected := range contract.Frame {
		if key == "message" {
			continue
		}
		if !reflect.DeepEqual(frame[key], expected) {
			t.Fatalf("recorded frame field %s changed", key)
		}
	}
	if frame["msg_id"] != msg.ID || frame["message"].(map[string]any)["role"] != contract.Frame["message"].(map[string]any)["role"] {
		t.Fatal("frame lost its identity or role")
	}
	message := frame["message"].(map[string]any)["content"].(string)
	if !strings.Contains(message, "From: Codex") || !strings.Contains(message, "exact body") || strings.Contains(message, msg.From) {
		t.Fatal(message)
	}
	if strings.Contains(message, "capability is") {
		t.Fatal("private capability leaked")
	}
}

func TestClaudeFrameSizeAndMissingEndpoint(t *testing.T) {
	listener, endpoint := listenClaudeSocket(t)
	adapter := Adapter{}
	for _, test := range []struct {
		name     string
		endpoint *bus.Endpoint
		body     string
	}{
		{"missing endpoint", nil, "hello"},
		{"oversized escaped frame", endpoint, strings.Repeat("\x01", 16*1024)},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := adapter.Deliver(context.Background(), bus.Message{ID: newTestID(), Body: test.body}, bus.Session{Endpoint: test.endpoint})
			if receipt.Status != bus.StatusNotSent || receipt.Error == "" || receipt.At == "" {
				t.Fatalf("invalid delivery was not rejected: %+v", receipt)
			}
		})
	}
	if err := listener.SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if conn, err := listener.Accept(); err == nil {
		conn.Close()
		t.Fatal("rejected delivery still dialed the native socket")
	} else {
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal("failed to observe the socket", err)
		}
	}
}
