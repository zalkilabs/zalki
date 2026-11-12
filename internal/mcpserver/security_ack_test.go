package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestMCPAckUniformPrivateRefusal(t *testing.T) {
	service, author, key, other, _, plugin := newMCPInboxFixture(t)
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		return model.NewReceipt(model.StatusSubmitted)
	}
	send := func(to string) model.Message {
		t.Helper()
		msg, err := service.Send(t.Context(), bus.SendRequest{Actor: author.ID, Capability: key, To: to, Body: "PRIVATE ACK PAYLOAD"})
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}
	foreign, pluginMsg := send(other.ID), send(plugin)
	corrupt := "40000000-0000-4000-8000-000000000099"
	if err := os.WriteFile(filepath.Join(service.Store.Dir, "messages", corrupt+".json"), []byte(`{"body":"PRIVATE PARTIAL","receipt":{"error":"/secret/path"},`), 0600); err != nil {
		t.Fatal(err)
	}
	client, ctx := testClient(t, service)
	var baseline map[string]json.RawMessage
	for _, id := range []string{"40000000-0000-4000-8000-000000000098", foreign.ID, pluginMsg.ID, corrupt} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "ack_message", Arguments: map[string]any{"actor": author.ID, "capability": key, "message_id": id}})
		if err != nil || !result.IsError {
			t.Fatal("ACK refusal missing", err)
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		projection := map[string]json.RawMessage{"data": fields["data"], "error": fields["error"]}
		if baseline == nil {
			baseline = projection
		} else if !reflect.DeepEqual(baseline, projection) {
			t.Fatalf("unknown vs private ACK leak: %s", raw)
		}
		var out Output[MessageReceipt]
		if err := json.Unmarshal(raw, &out); err != nil || out.Data.ID != "" || out.Data.Status != "" || out.Data.Acknowledged {
			t.Fatal("ACK exposed unauthorized receipt", err)
		}
	}
}

func TestMCPCodexQueueOffReceiptIsUnavailable(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	service := newTestService(t)
	author, key, target, _ := registerPair(t, service.Store, "cursor", "50000000-0000-4000-8000-000000000001", "codex", "50000000-0000-4000-8000-000000000002")
	client, ctx := testClient(t, service)
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"actor": author.ID, "capability": key, "to": target.ID, "body": "PRIVATE QUEUE BODY"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out Output[MessageReceipt]
	if err := json.Unmarshal(raw, &out); err != nil || out.Data.Status != model.StatusNotSent || out.Data.Acknowledged || out.Data.Code != model.CodeUnavailable || !strings.Contains(out.Data.Error, "Agents must not change this setting.") || strings.Contains(string(raw), "PRIVATE QUEUE BODY") {
		t.Fatalf("default-off MCP receipt: %s %v", raw, err)
	}
}
