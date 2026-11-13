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
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestMCPPluginInboxStoredReceiptAndPrivateErrors(t *testing.T) {
	service, author, key, other, otherKey, to := newMCPInboxFixture(t)
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		t.Error("plugin inbox send invoked native delivery")
		return model.NewReceipt(model.StatusSubmitted)
	}
	client, ctx := testClient(t, service)
	id := "30000000-0000-4000-8000-000000000051"
	body := "MCP-PLUGIN-INBOX-PRIVATE-BODY"
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"actor": author.ID, "capability": key, "to": to, "message_id": id, "body": body}})
	if err != nil || result.IsError {
		t.Fatal("plugin storage failed", err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var receipt Output[MessageReceipt]
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt.Data, MessageReceipt{ID: id, Status: model.StatusSubmitted, Note: "Stored, not delivered; owner reading is a later command. Not acknowledged."}) || strings.Contains(string(raw), body) || strings.Contains(string(raw), key) {
		t.Fatal("stored-only receipt leaked body or promised delivery")
	}
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{"actor": author.ID, "capability": key, "message_id": id, "include_body": true}})
	if err != nil || result.IsError {
		t.Fatal("author exact read failed", err)
	}
	raw, _ = json.Marshal(result.StructuredContent)
	var page Output[MessagePage]
	if err = json.Unmarshal(raw, &page); err != nil || len(page.Data.Messages) != 1 || page.Data.Messages[0].Body != body {
		t.Fatal("author lost own body", err)
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"list_messages", map[string]any{"actor": other.ID, "capability": otherKey, "message_id": id, "include_body": true}},
		{"list_messages", map[string]any{"actor": to, "capability": key, "message_id": id, "include_body": true}},
		{"ack_message", map[string]any{"actor": author.ID, "capability": key, "message_id": id}},
		{"ack_message", map[string]any{"actor": other.ID, "capability": otherKey, "message_id": id}},
		{"reply_message", map[string]any{"actor": other.ID, "capability": otherKey, "message_id": id, "body": "cannot consume plugin inbox"}},
		{"watch_agent", map[string]any{"actor": author.ID, "capability": key, "to": to}},
	} {
		checkInboxMCPFailure(t, client, ctx, call.name, call.args, body, key, otherKey)
	}
}

func checkInboxMCPFailure(t *testing.T, client *mcp.ClientSession, ctx context.Context, name string, args map[string]any, body, key, otherKey string) {
	t.Helper()

	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || !result.IsError {
		t.Fatal("native plugin operation unexpectedly succeeded", name, err)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), body) || strings.Contains(string(raw), key) || strings.Contains(string(raw), otherKey) {
		t.Fatal("native failure leaked plugin body or capability", name)
	}
	if name == "ack_message" {
		var out Output[MessageReceipt]
		data, _ := json.Marshal(result.StructuredContent)
		if err = json.Unmarshal(data, &out); err != nil || out.Data.ID != "" || out.Data.Status != "" || out.Data.Acknowledged {
			t.Fatal("failed plugin ACK exposed message metadata")
		}
	}

}

func newMCPInboxFixture(t *testing.T) (*bus.Service, model.Session, string, model.Session, string, string) {
	t.Helper()
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	service := newTestService(t)
	service.PluginInbox = pluginfeed.AdmitPluginInbox
	author, key, other, otherKey := registerPair(t, service.Store, "cursor", "00000000-0000-4000-8000-000000000051", "codex", "00000000-0000-4000-8000-000000000052")
	executable := filepath.Join(t.TempDir(), "private-plugin")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(service.Store, "inbox-fixture")
	if err := feed.AddExternalPlugin(executable, nil); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true, Receive: true}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	to := "plugin:" + status.ID

	return service, author, key, other, otherKey, to
}
