package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// End to end through the real MCP server: list_messages returns the readable
// messages and unreadable=1 when one message file cannot be read, and the
// healthy output has no unreadable key.
func TestListMessagesReportsUnreadableFilesInStructuredContent(t *testing.T) {
	service := newTestService(t)
	sender, senderKey, target, targetKey := registerPair(t, service.Store, "codex", "00000000-0000-4000-8000-000000000201", "cursor", "00000000-0000-4000-8000-000000000202")
	msg, _, _, err := service.Store.ReserveMessage(bus.SendRequest{Actor: sender.ID, Capability: senderKey, To: target.ID, MessageID: "00000000-0000-4000-8000-000000000203", Body: "readable"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.Finish(msg.ID, model.Receipt{Status: model.StatusPendingActivity}); err != nil {
		t.Fatal(err)
	}
	client, ctx := testClient(t, service)
	list := func() string {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{"actor": target.ID, "capability": targetKey}})
		if err != nil || result.IsError {
			t.Fatalf("list_messages failed: %v %+v", err, result)
		}
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if healthy := list(); strings.Contains(healthy, "unreadable") || !strings.Contains(healthy, msg.ID) {
		t.Fatalf("healthy output = %s", healthy)
	}
	bad := filepath.Join(service.Store.Dir, "messages", "00000000-0000-4000-8000-000000000204.json")
	if err := os.WriteFile(bad, []byte(`{"schema":1,"id":"`), 0600); err != nil {
		t.Fatal(err)
	}
	var output struct {
		Data MessagePage `json:"data"`
	}
	raw := list()
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		t.Fatal(err)
	}
	if output.Data.Unreadable != 1 || len(output.Data.Messages) != 1 || output.Data.Messages[0].ID != msg.ID {
		t.Fatalf("output with one unreadable file = %s", raw)
	}
	if strings.Contains(raw, "000000000204") {
		t.Fatalf("output names the unreadable file: %s", raw)
	}
}

// The 24 KiB page budget also counts the unreadable field: a page filled to the
// exact limit without it must not pass the limit when the count is added.
func TestMessagePageBudgetCountsTheUnreadableField(t *testing.T) {
	used := len(`{"messages":[],"more":false,"next_cursor":"00000000-0000-4000-8000-000000000000"}`)
	view := func(n int) model.Message {
		return model.Message{ID: "00000000-0000-4000-8000-000000000001", Body: strings.Repeat("a", n)}
	}
	n := 1000
	for {
		encoded, _ := json.Marshal(messageViews([]model.Message{view(n)})[0])
		if used+len(encoded)+1 >= messagePageBytes {
			if used+len(encoded)+1 != messagePageBytes {
				t.Fatalf("fixture could not hit the exact limit at n=%d", n)
			}
			break
		}
		n++
	}
	second := model.Message{ID: "00000000-0000-4000-8000-000000000002", Body: "next"}
	page := messagePage(bus.MessageBatch{Messages: []model.Message{view(n), second}, More: true, Unreadable: 1}, false)
	encoded, _ := json.Marshal(page)
	if len(encoded) > messagePageBytes || page.Unreadable != 1 {
		t.Fatalf("page is %d bytes with unreadable=%d, over the %d limit", len(encoded), page.Unreadable, messagePageBytes)
	}
}
