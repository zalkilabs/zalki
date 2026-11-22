package mcpserver

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestMissingCredentialsAndBoundaryIDsNameTheFix(t *testing.T) {
	client, ctx := testClient(t, &bus.Service{})
	for name, test := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"no credentials":    {"send_message", map[string]any{"to": "x", "body": "b"}, "actor and capability is required in every send_message call; use the actor and capability from your newest SessionStart"},
		"empty capability":  {"watch_agent", map[string]any{"actor": "a", "capability": "", "to": "x"}, "capability is required"},
		"boundary as reply": {"reply_message", map[string]any{"actor": "a", "capability": "k", "message_id": "0123456789abcdef0123456789abcdef", "body": "b"}, "the Boundary value is not an ID"},
		"boundary as ack":   {"ack_message", map[string]any{"actor": "a", "capability": "k", "message_id": "0123456789abcdef0123456789abcdef"}, "the Boundary value is not an ID"},
		"boundary as read":  {"list_messages", map[string]any{"actor": "a", "capability": "k", "message_id": "0123456789abcdef0123456789abcdef"}, "the Boundary value is not an ID"},
	} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: test.tool, Arguments: test.args})
		if err != nil || !result.IsError || len(result.Content) == 0 {
			t.Fatalf("%s: result %+v, error %v", name, result, err)
		}
		text := result.Content[0].(*mcp.TextContent).Text
		if !strings.Contains(text, test.want) || strings.Contains(text, "0123456789abcdef") {
			t.Fatalf("%s: text %q, want %q and no echoed value", name, text, test.want)
		}
	}
}

func TestToolDescriptionsNameTheRequiredKeys(t *testing.T) {
	client, ctx := testClient(t, &bus.Service{})
	result, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range result.Tools {
		if tool.Name == "list_agents" {
			continue
		}
		keys := []string{"actor", "capability"}
		switch tool.Name {
		case "send_message":
			keys = append(keys, "to", "body")
		case "reply_message":
			keys = append(keys, "message_id", "body")
		case "ack_message":
			keys = append(keys, "message_id")
		case "watch_agent":
			keys = append(keys, "to")
		case "set_task":
			keys = append(keys, "task")
		}
		for _, key := range keys {
			if !strings.Contains(tool.Description, key) {
				t.Fatalf("%s description does not name %q: %s", tool.Name, key, tool.Description)
			}
		}
	}
}

// An empty message_id on list_messages means "not given": the call reads the inbox.
func TestListMessagesEmptyMessageIDReadsTheInbox(t *testing.T) {
	fake := &fakeBus{}
	client, ctx := fakeClient(t, fake)
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{"actor": "a", "capability": "k", "message_id": ""}})
	if err != nil || result.IsError {
		t.Fatalf("result %+v, error %v", result, err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "ReadMessages" || fake.query.MessageID != "" {
		t.Fatalf("calls %v query %+v, want one inbox read", fake.calls, fake.query)
	}
}
