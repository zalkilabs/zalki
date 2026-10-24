package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestBinaryMCPLoggingStaysOffProtocolStdout(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("set ZALKI_TEST_BINARY for stdio logging integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	state := filepath.Join(t.TempDir(), "state")
	store := bus.Store{Dir: state}
	actor, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000041", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary, "--state", state, "--log-level", "debug", "--log-file", "-", "mcp")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "logging-fixture", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal("stdio handshake failed with logging enabled", err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{"actor": actor.ID, "capability": key}})
	if err != nil || result.IsError {
		t.Fatal("stdio tool exchange failed with logging enabled", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), key) || strings.Contains(stderr.String(), "capability") {
		t.Fatal("MCP logging exposed credentials")
	}
	scanner := bufio.NewScanner(bytes.NewReader(stderr.Bytes()))
	found := false
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil || row["component"] != "mcp" || row["pid"] == nil {
			t.Fatal("non-JSON or uncorrelated stderr event", err)
		}
		if row["msg"] == "tool.list_messages" && row["actor"] == actor.ID {
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("MCP tool operation was not logged")
	}
}
