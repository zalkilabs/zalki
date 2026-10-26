package adapters

import (
	"bytes"
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

func TestClaudeRegistrationIdentityAndHealth(t *testing.T) {
	listener, endpoint := listenClaudeSocket(t)
	adapter := claudeAdapter{}
	id := newTestID()
	t.Setenv("CLAUDE_CODE_SESSION_ID", strings.ToUpper(id))
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", endpoint.Path)
	registration, err := adapter.RegisterHook(bus.HookInput{SessionID: id})
	if err != nil || registration.Endpoint == nil || *registration.Endpoint != *endpoint || registration.PreserveGeneration {
		t.Fatal("matching native identity did not bind the exact endpoint", err)
	}
	if transport, _ := adapter.Health(nil, bus.Session{Endpoint: endpoint}); transport != "bound_socket_present" {
		t.Fatal("registered endpoint is unavailable", transport)
	}
	// Verify actual startup composition binds this endpoint, rather than only
	// checking RegisterHook in isolation from Service and the Builtin registry.
	store := adapterTestStore(t)
	service := bus.Service{Store: store}
	input, err := json.Marshal(bus.HookInput{SessionID: id, CWD: "/workspace", Event: "SessionStart"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := service.Hook("claude", bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	current := adapterTestSession(t, store, "claude:"+id)
	if current.Endpoint == nil || *current.Endpoint != *endpoint || !bytes.Contains(output.Bytes(), []byte(current.ID)) {
		t.Fatal("startup did not bind and announce the native Claude session")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", newTestID())
	if err := service.Hook("claude", bytes.NewReader(input), &bytes.Buffer{}); err == nil {
		t.Fatal("accepted a hook for another native session")
	}
	if after := adapterTestSession(t, store, current.ID); after.Generation != current.Generation {
		t.Fatal("rejected startup replaced the receiving registration")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if _, err := adapter.RegisterHook(bus.HookInput{SessionID: id}); err != nil {
		t.Fatal("an absent environment identity must allow the hook identity", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint.Path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if transport, _ := adapter.Health(nil, bus.Session{Endpoint: endpoint}); transport != "socket_unavailable" {
		t.Fatal("health accepted a replaced endpoint", transport)
	}
	receipt := adapter.Deliver(context.Background(), bus.Message{ID: newTestID(), Body: "stale binding"}, bus.Session{Endpoint: endpoint})
	if receipt.Status != bus.StatusNotSent || receipt.Error == "" || receipt.At == "" {
		t.Fatalf("delivery accepted a replaced endpoint: %+v", receipt)
	}
}

func TestClaudeFrameSizeAndMissingEndpoint(t *testing.T) {
	listener, endpoint := listenClaudeSocket(t)
	adapter := claudeAdapter{}
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

func TestClaudeServiceWritesOnceAndPreservesSenderLabel(t *testing.T) {
	listener, endpoint := listenClaudeSocket(t)
	store := adapterTestStore(t)
	from, key := registerAdapterSession(t, store, "codex")
	to, _, err := store.Register("claude", newTestID(), "/workspace", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	request := bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newTestID(), Body: "write once"}
	for range 2 {
		msg, err := service.Send(context.Background(), request)
		if err != nil || msg.Status != bus.StatusTransportWritten || msg.FromLabel != "Codex" {
			t.Fatal("native delivery or idempotent retry failed", msg.Status, err)
		}
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var frame struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(conn).Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(frame.Message.Content, "From: Codex") || strings.Contains(frame.Message.Content, key) {
		t.Fatal("sender label or capability privacy changed")
	}
	if err := listener.SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if conn, err := listener.Accept(); err == nil {
		conn.Close()
		t.Fatal("idempotent retry dialed Claude a second time")
	} else {
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal("failed to observe the socket", err)
		}
	}
}
