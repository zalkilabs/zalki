package mcpserver

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func newTestStore(t *testing.T) bus.Store {
	t.Helper()
	return bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: adapters.Builtin()}
}

func newTestService(t *testing.T) *bus.Service {
	t.Helper()
	return &bus.Service{Store: newTestStore(t)}
}

// MCP fixtures use fixed native IDs so request sizes and correlation stay stable.
func registerPair(t *testing.T, store bus.Store, senderKind, senderID, targetKind, targetID string) (bus.Session, string, bus.Session, string) {
	t.Helper()
	sender, senderKey, err := store.Register(senderKind, senderID, "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	target, targetKey, err := store.Register(targetKind, targetID, "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	return sender, senderKey, target, targetKey
}

func testClient(t *testing.T, service *bus.Service) (*mcp.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ct, st := mcp.NewInMemoryTransports()
	server, err := New(service).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "ergonomics", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, ctx
}
