package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestMCPAliasesBeforeValidationAndWithoutSideEffectsOnConflict(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, t.TempDir())
	}
	service := newTestService(t)
	store := service.Store
	from, key, to, toKey := registerPair(t, store, "cursor", "00000000-0000-4000-8000-000000000031", "codex", "00000000-0000-4000-8000-000000000032")
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		return model.NewReceipt(model.StatusSubmitted)
	}
	client, ctx := testClient(t, service)
	call := func(tool string, args any) *mcp.CallToolResult {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := call("send_message", map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "message": "hello"})
	if result.IsError {
		t.Fatalf("alias-only send failed: %+v", result)
	}
	var sent Output[MessageReceipt]
	encoded, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(encoded, &sent); err != nil || sent.Data.ID == "" {
		t.Fatal("send lost its generated ID", err)
	}
	// The spellings decode to equal strings despite different JSON encodings.
	args, _ := json.Marshal(map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "message_id": sent.Data.ID, "body": "hello"})
	raw := strings.TrimSuffix(string(args), "}") + `,"message":"\u0068ello"}`
	if result := call("send_message", json.RawMessage(raw)); result.IsError {
		t.Fatalf("equal canonical/alias retry failed: %+v", result)
	}
	for _, test := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"send_message", map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "body": "secret-body", "message": "secret-alias"}, "must match"},
		{"send_message", map[string]any{"message": 7}, "must be a string"},
		{"send_message", map[string]any{"body": nil, "message": "secret-alias"}, "must be a string"},
		{"send_message", map[string]any{"message": nil}, "must be a string"},
		{"reply_message", map[string]any{"actor": to.ID, "capability": toKey, "message_id": sent.Data.ID, "reply_to": "secret-parent", "message": "secret-body"}, "must match"},
		{"reply_message", map[string]any{"reply_to": false}, "must be a string"},
		{"send_message", map[string]any{"message": "secret-body", "unexpected": "secret-extra"}, "unknown fields"},
		{"ack_message", map[string]any{"reply_to": "secret-parent"}, "unknown fields"},
	} {
		result := call(test.tool, test.args)
		encoded, _ := json.Marshal(result)
		if !result.IsError || !strings.Contains(string(encoded), test.want) {
			t.Fatalf("alias failure lacks %q: %s", test.want, encoded)
		}
		for _, secret := range []string{"secret-body", "secret-alias", "secret-parent", "secret-extra", key, toKey} {
			if strings.Contains(string(encoded), secret) {
				t.Fatal("alias error echoed a private value")
			}
		}
	}
	// A rejected reply must not acknowledge its parent.
	page, err := store.ReadMessages(bus.MessageQuery{Actor: from.ID, Capability: key, Sent: true})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].AcknowledgedAt != "" {
		t.Fatal("invalid aliases changed delivery or acknowledgement state", err)
	}
	result = call("reply_message", map[string]any{"actor": to.ID, "capability": toKey, "reply_to": sent.Data.ID, "message": "reviewed"})
	if result.IsError {
		t.Fatalf("alias-only reply failed: %+v", result)
	}
	var replied Output[MessageReceipt]
	encoded, _ = json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(encoded, &replied); err != nil || replied.Data.ID == "" || replied.Data.ID == sent.Data.ID {
		t.Fatal("reply alias lost the outgoing identity", err)
	}
	result = call("reply_message", map[string]any{"actor": to.ID, "capability": toKey, "reply_to": sent.Data.ID, "message_id": sent.Data.ID, "body": "reviewed", "message": "reviewed"})
	encoded, _ = json.Marshal(result.StructuredContent)
	var retry Output[MessageReceipt]
	if err := json.Unmarshal(encoded, &retry); err != nil || result.IsError || retry.Data.ID != replied.Data.ID {
		t.Fatal("equal aliases changed reply retry identity", err)
	}
	tools, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "send_message" && tool.Name != "reply_message" {
			continue
		}
		schema, _ := json.Marshal(tool.InputSchema)
		for _, alias := range []string{`"message":`, `"reply_to":`} {
			if strings.Contains(string(schema), alias) {
				t.Fatal("aliases inflated the canonical input schema")
			}
		}
	}
}

func TestArgumentHintsKeepStrictSchemasWithoutEchoingValues(t *testing.T) {
	store := newTestStore(t)
	client, ctx := testClient(t, &bus.Service{Store: store})
	for _, test := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"list_agents", map[string]any{"unknown": "secret-value"}, []string{"unknown", "provide actor and capability together"}},
		{"send_message", map[string]any{"text": "secret-value"}, []string{`"text"`, "accepts actor, body, capability, message_id, to", "Use body", "retries safe"}},
		{"reply_message", map[string]any{"parent": "secret-value"}, []string{`"parent"`, "message_id is the incoming message", "body is the reply"}},
		{"send_message", map[string]any{"sent": true}, []string{`"sent"`, "accepts actor, body, capability, message_id, to"}},
		{"list_messages", map[string]any{"unknown": "secret-value"}, []string{`"unknown"`, "accepts actor", "sent"}},
		{"set_task", map[string]any{"note": "secret-value"}, []string{`"note"`, "accepts actor, capability, task"}},
	} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: test.tool, Arguments: test.args})
		if err != nil || !result.IsError {
			t.Fatalf("%s accepted unknown fields: %+v %v", test.tool, result, err)
		}
		encoded, _ := json.Marshal(result)
		text := ""
		for _, block := range result.Content {
			if content, ok := block.(*mcp.TextContent); ok {
				text += content.Text
			}
		}
		for _, want := range test.want {
			if !strings.Contains(text, want) {
				t.Fatalf("%s lacks helpful hint %q: %s", test.tool, want, text)
			}
		}
		if strings.Contains(string(encoded), "secret-value") {
			t.Fatal("argument hint echoed private values")
		}
	}
}
