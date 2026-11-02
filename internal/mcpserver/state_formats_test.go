package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestBinarySTDIOReadsSurviveFutureStateFormats(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("set ZALKI_TEST_BINARY for stdio integration")
	}
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(name, t.TempDir())
	}
	service := newTestService(t)
	store := service.Store
	from, key, to, _ := registerPair(t, store, "cursor", "00000000-0000-4000-8000-000000000071", "cursor", "00000000-0000-4000-8000-000000000072")
	for i := range 3 {
		if _, err := service.Send(t.Context(), bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, Body: fmt.Sprintf("fixture %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	marker := []byte(`{"schema":1,"formats":{"sessions":99},"future_marker_field":true}`)
	markerPath := filepath.Join(store.Dir, "state-formats.json")
	if err := os.WriteFile(markerPath, marker, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--state", store.Dir, "--log-level", "off", "mcp")
	client, err := mcp.NewClient(&mcp.Implementation{Name: "read-only-fixture", Version: "0"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal("future marker prevented MCP startup", err)
	}
	defer client.Close()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatal("read-only discovery failed", err)
	}
	count := 0
	cursor := ""
	for {
		result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{"actor": from.ID, "capability": key, "sent": true, "limit": 1, "cursor": cursor}})
		if err != nil || result.IsError {
			t.Fatal("read-only paging failed", err)
		}
		var page Output[MessagePage]
		raw, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		count += len(page.Data.Messages)
		if !page.Data.More {
			break
		}
		if page.Data.NextCursor == "" || page.Data.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.Data.NextCursor
	}
	if count != 3 {
		t.Fatal("read-only pages lost messages", count)
	}
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "set_task", Arguments: map[string]any{"actor": from.ID, "capability": key, "task": "must not persist"}})
	if err != nil || !result.IsError {
		t.Fatal("future marker allowed mutation", err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if !bytes.Contains(raw, []byte("upgrade zalki")) {
		t.Fatal("write failure lacked upgrade guidance", string(raw))
	}
	for _, name := range []string{"runtimes", "watches", "watch-observer.stamp"} {
		if _, err := os.Stat(filepath.Join(store.Dir, name)); !os.IsNotExist(err) {
			t.Fatal("fenced startup created state", name, err)
		}
	}
	if after, _ := os.ReadFile(markerPath); !bytes.Equal(marker, after) {
		t.Fatal("MCP rewrote future marker")
	}
	files, err := os.ReadDir(filepath.Join(store.Dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Join(store.Dir, "sessions", file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "must not persist") {
			t.Fatal("fenced task was saved")
		}
	}
}
