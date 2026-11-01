package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestMCPAliasesBeforeValidationAndWithoutSideEffectsOnConflict(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, t.TempDir())
	}
	store := bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: adapters.Builtin()}
	from, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000031", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, toKey, err := store.Register("codex", "00000000-0000-4000-8000-000000000032", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	service := &bus.Service{Store: store, Deliver: func(context.Context, bus.Message, bus.Session) bus.Receipt {
		return bus.NewReceipt(bus.StatusSubmitted)
	}}
	client, ctx := ergonomicsClient(t, service)
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

func TestMCPDiscoveryMarksOnlyAuthenticatedCaller(t *testing.T) {
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	store := bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: adapters.Builtin()}
	actors := make([]string, 2)
	keys := make([]string, 2)
	for i, id := range []string{"00000000-0000-4000-8000-000000000041", "00000000-0000-4000-8000-000000000042"} {
		v, key, err := store.Register("cursor", id, "/fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		actors[i], keys[i] = v.ID, key
	}
	client, ctx := ergonomicsClient(t, &bus.Service{Store: store})
	for _, caller := range []int{-1, 0, 1, -1} {
		args := map[string]any{}
		if caller >= 0 {
			args["actor"], args["capability"] = actors[caller], keys[caller]
		}
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("discovery failed: %+v %v", result, err)
		}
		var output Output[[]bus.AgentInfo]
		encoded, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(encoded, &output); err != nil || len(output.Data) != 2 {
			t.Fatal("discovery lost rows", err)
		}
		for _, row := range output.Data {
			wantSelf := caller >= 0 && row.ID == actors[caller]
			if row.Self != wantSelf {
				t.Fatal("self marker leaked across callers")
			}
		}
	}
	for _, args := range []map[string]any{
		{"actor": actors[0]},
		{"capability": keys[0]},
		{"actor": "", "capability": ""},
		{"actor": actors[0], "capability": ""},
		{"actor": actors[0], "capability": "wrong"},
		{"actor": actors[0], "capability": keys[1]},
	} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: args})
		if err != nil || !result.IsError {
			t.Fatal("invalid discovery credentials fell back to an anonymous roster", err)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "/fixture") || strings.Contains(string(encoded), keys[0]) || strings.Contains(string(encoded), keys[1]) {
			t.Fatal("invalid discovery returned a roster or credentials")
		}
	}
}
