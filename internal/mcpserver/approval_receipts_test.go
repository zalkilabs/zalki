package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestMCPApprovalReceiptsDistinguishHeldMessagesFromLegacyRefusals(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, t.TempDir())
	}
	store := bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: adapters.Builtin()}
	from, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000021", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := store.Register("codex", "00000000-0000-4000-8000-000000000022", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(to.ID, bus.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service := &bus.Service{Store: store, Deliver: func(context.Context, bus.Message, bus.Session) bus.Receipt {
		calls++
		return bus.NewReceipt(bus.StatusSubmitted)
	}}
	client, ctx := ergonomicsClient(t, service)
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
	if held.Status != bus.StatusApprovalBlocked || held.Note != "Queued until the target's permission prompt is answered." {
		t.Fatalf("held receipt: status=%s note=%q", held.Status, held.Note)
	}
	legacy := bus.Message{Schema: 1, ID: "00000000-0000-4000-8000-000000000023", From: from.ID, To: to.ID,
		Body: "legacy receipt fixture", TargetGeneration: to.Generation, CreatedAt: "2026-11-01T00:00:00Z", Status: bus.StatusApprovalBlocked}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", legacy.ID+".json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	refused := send(legacy.ID, legacy.Body)
	if refused.Status != bus.StatusApprovalBlocked || refused.Note != "Not sent: target awaiting approval; no automatic retry." {
		t.Fatalf("legacy receipt: status=%s note=%q", refused.Status, refused.Note)
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
		var stored bus.Message
		if err := json.Unmarshal(data, &stored); err != nil {
			t.Fatal(err)
		}
		if stored.Held != test.held {
			t.Fatalf("message %s held=%t, want %t", test.id, stored.Held, test.held)
		}
	}
}
