package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestCollaborationCost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := bus.Store{Dir: filepath.Join(t.TempDir(), "state")}
	author, authorKey, err := store.Register("cursor", "00000000-0000-4000-8000-000000000101", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, reviewerKey, err := store.Register("cursor", "00000000-0000-4000-8000-000000000102", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(&bus.Service{Store: store}).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "cost-fixture", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, _ := json.Marshal(tools)
	metrics := struct {
		Kind             string `json:"kind"`
		ToolCalls        int    `json:"tool_calls"`
		LogicalHandoffs  int    `json:"logical_handoffs"`
		SchemaBytes      int    `json:"schema_bytes"`
		InstructionBytes int    `json:"instruction_bytes"`
		RequestBytes     int    `json:"request_bytes"`
		ResponseBytes    int    `json:"response_bytes"`
		ModelTokens      *int   `json:"model_tokens"`
		ModelTurns       *int   `json:"model_turns"`
	}{Kind: "scripted_mcp_review_revision", LogicalHandoffs: 4, SchemaBytes: len(schema), InstructionBytes: len(session.InitializeResult().Instructions)}
	call := func(name string, args map[string]any) json.RawMessage {
		t.Helper()
		params := &mcp.CallToolParams{Name: name, Arguments: args}
		request, _ := json.Marshal(params)
		metrics.RequestBytes += len(request)
		metrics.ToolCalls++
		result, err := session.CallTool(ctx, params)
		if err != nil || result.IsError {
			t.Fatalf("%s failed: %v", name, err)
		}
		response, _ := json.Marshal(result.StructuredContent)
		metrics.ResponseBytes += len(response)
		var output struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(response, &output); err != nil {
			t.Fatal(err)
		}
		return output.Data
	}
	read := func(actor, key, id, body string) {
		t.Helper()
		data := call("list_messages", map[string]any{"actor": actor, "capability": key, "message_id": id})
		var page MessagePage
		if err := json.Unmarshal(data, &page); err != nil || len(page.Messages) != 1 || page.Messages[0].Body != body {
			t.Fatal("exact message body missing", err)
		}
	}
	requestID := "00000000-0000-4000-8000-000000000103"
	body := "Review this cache design: receipt metadata is cached after each read. The journal is authoritative, and other processes can acknowledge messages. Identify one correctness risk and a concrete fix."
	call("send_message", map[string]any{"actor": author.ID, "capability": authorKey, "to": reviewer.ID, "message_id": requestID, "body": body})
	read(reviewer.ID, reviewerKey, requestID, body)
	parent := requestID
	for i, text := range []string{
		"Risk: another process can ACK a message while the cache still says unread. Fix: invalidate through a durable journal revision before reusing cached metadata.",
		"Revision: exact-ID reads consult the canonical record directly. General inbox reads remain authoritative and bounded; no stale receipt cache is introduced.",
		"Accepted: the revision removes the stale-cache risk while preserving exact reply correlation. Keep a regression test for cross-process acknowledgements.",
	} {
		actor, key, recipient, recipientKey := reviewer.ID, reviewerKey, author.ID, authorKey
		if i == 1 {
			actor, key, recipient, recipientKey = author.ID, authorKey, reviewer.ID, reviewerKey
		}
		data := call("reply_message", map[string]any{"actor": actor, "capability": key, "message_id": parent, "body": text})
		var receipt MessageReceipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "body") {
			t.Fatal("mutation receipt echoed substantive content")
		}
		parent = receipt.ID
		read(recipient, recipientKey, parent, text)
	}
	call("ack_message", map[string]any{"actor": author.ID, "capability": authorKey, "message_id": parent})
	if metrics.ToolCalls != 9 {
		t.Fatalf("unexpected protocol overhead: %d calls", metrics.ToolCalls)
	}
	if metrics.ResponseBytes > 6*1024 {
		t.Fatalf("review workflow exceeded response budget: %d bytes", metrics.ResponseBytes)
	}
	encoded, _ := json.Marshal(metrics)
	t.Log(string(encoded))
}
