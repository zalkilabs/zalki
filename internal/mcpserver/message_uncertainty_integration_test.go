package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/model"
)

func uncertaintyCall[T any](t *testing.T, client *mcp.ClientSession, ctx context.Context, name string, args map[string]any) T {
	t.Helper()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || result.IsError {
		t.Fatalf("uncertainty %s failed: %v %+v", name, err, result)
	}
	raw, err := json.Marshal(result.StructuredContent)
	var value Output[T]
	if err != nil || json.Unmarshal(raw, &value) != nil || value.Error != nil {
		t.Fatal("invalid uncertainty projection", err)
	}
	return value.Data
}

func TestMCPUncertainSendHistoryAndAck(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusUnknownWrite, model.StatusUnknownTimeout} {
		t.Run(string(status), func(t *testing.T) { checkMCPUncertainFlow(t, status) })
	}
}

func checkMCPUncertainFlow(t *testing.T, status model.DeliveryStatus) {
	t.Helper()
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	service := newTestService(t)
	from, key, to, toKey := registerPair(t, service.Store, "codex", "00000000-0000-4000-8000-000000000011", "codex", "00000000-0000-4000-8000-000000000012")
	calls := 0
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		calls++
		return model.Receipt{Status: status, At: "2026-11-13T23:00:00Z", Error: "private-native-marker", Code: model.CodeUnavailable}
	}
	client, ctx := testClient(t, service)
	args := map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "message_id": uncertaintyID, "body": "private-body-marker"}
	receipt := uncertaintyCall[MessageReceipt](t, client, ctx, "send_message", args)
	if receipt.Status != status || receipt.ID != uncertaintyID || !strings.Contains(receipt.Error, "do not resend it without checking.") {
		t.Fatal("MCP uncertainty guidance/status missing")
	}
	repeated := uncertaintyCall[MessageReceipt](t, client, ctx, "send_message", args)
	if calls != 1 || repeated.ID != receipt.ID {
		t.Fatal("same-ID lookup retried native delivery")
	}
	checkMCPUncertainHistory(t, client, ctx, service.Store.Dir, from.ID, key, to.ID, toKey)
	ack := uncertaintyCall[MessageReceipt](t, client, ctx, "ack_message", map[string]any{"actor": to.ID, "capability": toKey, "message_id": receipt.ID})
	if !ack.Acknowledged || ack.Status != status || !strings.HasPrefix(ack.Error, "The receiver ACKed message ") {
		t.Fatal("MCP ACK lost independent uncertain outcome")
	}
	page := uncertaintyCall[MessagePage](t, client, ctx, "list_messages", map[string]any{"actor": from.ID, "capability": key, "message_id": receipt.ID})
	if len(page.Messages) != 1 || strings.Contains(page.Messages[0].Error, "since") || !strings.HasPrefix(page.Messages[0].Error, "The receiver ACKed message ") {
		t.Fatal("ACKed history still asks for ACK")
	}
	reply := uncertaintyCall[MessageReceipt](t, client, ctx, "reply_message", map[string]any{"actor": to.ID, "capability": toKey, "message_id": receipt.ID, "body": "reply"})
	if reply.Status != status || !strings.Contains(reply.Error, "do not resend it without checking.") {
		t.Fatal("reply uncertainty guidance missing")
	}
}

func checkMCPUncertainHistory(t *testing.T, client *mcp.ClientSession, ctx context.Context, state, from, key, to, toKey string) {
	t.Helper()
	path := filepath.Join(state, "messages", uncertaintyID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"actor": from, "capability": key, "sent": true},
		{"actor": to, "capability": toKey},
		{"actor": to, "capability": toKey, "message_id": uncertaintyID},
	} {
		page := uncertaintyCall[MessagePage](t, client, ctx, "list_messages", args)
		if len(page.Messages) != 1 || page.Messages[0].ID != uncertaintyID || !strings.Contains(page.Messages[0].Error, "Delivery unconfirmed since 2026-11-13T23:00:00Z") {
			t.Fatal("MCP history lost original uncertainty/time")
		}
		raw, _ := json.Marshal(page)
		if strings.Contains(string(raw), "private-native-marker") {
			t.Fatal("MCP exposed raw native diagnostic")
		}
		if args["actor"] == to && !strings.Contains(page.Messages[0].Error, "Sending another copy may duplicate it.") {
			t.Fatal("receiver guidance lost unconfirmed/duplication warning")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("history projection persisted display guidance", err)
	}
}
