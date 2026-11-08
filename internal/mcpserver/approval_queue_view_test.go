package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestMCPApprovalReceiptsDistinguishHeldMessagesFromLegacyRefusals(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, t.TempDir())
	}
	service := newTestService(t)
	store := service.Store
	from, key, to, _ := registerPair(t, store, "cursor", "00000000-0000-4000-8000-000000000021", "codex", "00000000-0000-4000-8000-000000000022")
	if err := store.Update(to.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		calls++
		return model.NewReceipt(model.StatusSubmitted)
	}
	client, ctx := testClient(t, service)
	send := func(id, body string) MessageReceipt {
		t.Helper()
		args := map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "body": body}
		if id != "" {
			args["message_id"] = id
		}
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatal("send returned an MCP error")
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var output Output[MessageReceipt]
		if err := json.Unmarshal(encoded, &output); err != nil {
			t.Fatal(err)
		}
		return output.Data
	}
	held := send("", "held receipt fixture")
	if held.Status != model.StatusQueuedForApproval || held.Note != "Queued until the target's permission prompt is answered." || held.Error != "" {
		t.Fatalf("held receipt: status=%s note=%q error=%q", held.Status, held.Note, held.Error)
	}
	legacy := model.Message{Schema: 1, ID: "00000000-0000-4000-8000-000000000023", From: from.ID, To: to.ID,
		Receipt: &model.Receipt{Status: model.StatusApprovalBlocked, Error: "queued until the permission prompt is answered"},
		Body:    "legacy receipt fixture", TargetGeneration: to.Generation, CreatedAt: "2026-11-01T00:00:00Z", Status: model.StatusApprovalBlocked}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", legacy.ID+".json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	refused := send(legacy.ID, legacy.Body)
	if refused.Status != model.StatusApprovalBlocked || refused.Note != "Not sent: target awaiting approval; no automatic retry." {
		t.Fatalf("legacy receipt: status=%s note=%q", refused.Status, refused.Note)
	}
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{
		"actor": from.ID, "capability": key, "message_id": held.ID,
	}})
	if err != nil || result.IsError {
		t.Fatal("held message lookup failed", err)
	}
	var page Output[MessagePage]
	view, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(view, &page); err != nil || len(page.Data.Messages) != 1 || page.Data.Messages[0].Status != model.StatusQueuedForApproval || page.Data.Messages[0].Error != "" {
		t.Fatal("held history lost its derived queue view", err)
	}
	if calls != 0 {
		t.Fatal("transport ran while approval was pending")
	}
	for _, test := range []struct {
		id   string
		held bool
	}{{held.ID, true}, {legacy.ID, false}} {
		data, err := os.ReadFile(filepath.Join(store.Dir, "messages", test.id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var stored model.Message
		if err := json.Unmarshal(data, &stored); err != nil {
			t.Fatal(err)
		}
		if stored.Status != model.StatusApprovalBlocked || stored.Held != test.held {
			t.Fatalf("message %s held=%t, want %t", test.id, stored.Held, test.held)
		}
	}
}
