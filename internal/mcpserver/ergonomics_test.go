package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func ergonomicsClient(t *testing.T, service *bus.Service) (*mcp.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ct, st := mcp.NewInMemoryTransports()
	server, err := New(service).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "ergonomics", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, ctx
}

func TestArgumentHintsKeepStrictSchemasWithoutEchoingValues(t *testing.T) {
	store := bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: adapters.Builtin()}
	client, ctx := ergonomicsClient(t, &bus.Service{Store: store})
	for _, test := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"list_agents", map[string]any{"actor": "secret-value", "capability": "secret-value"}, []string{"takes no arguments", "omit actor and capability"}},
		{"send_message", map[string]any{"message": "secret-value"}, []string{`"message"`, "accepts actor, body, capability, message_id, to", "Use body", "retries safe"}},
		{"reply_message", map[string]any{"reply_to": "secret-value"}, []string{`"reply_to"`, "message_id is the incoming message", "body is the reply"}},
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

func TestMCPGeneratedSendIDReplyCorrelationAndSentReceipts(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, t.TempDir())
	}
	store := bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: adapters.Builtin()}
	from, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000011", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, toKey, err := store.Register("codex", "00000000-0000-4000-8000-000000000012", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(to.ID, bus.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service := &bus.Service{Store: store, Deliver: func(context.Context, bus.Message, bus.Session) bus.Receipt {
		calls++
		return bus.NewReceipt(bus.StatusSubmitted)
	}}
	client, ctx := ergonomicsClient(t, service)
	call := func(name string, args map[string]any, out any) {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("%s failed: %+v %v", name, result, err)
		}
		encoded, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(encoded, out); err != nil {
			t.Fatal(err)
		}
	}
	args := map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "body": "review"}
	var first Output[MessageReceipt]
	call("send_message", args, &first)
	if _, err := bus.UUID(first.Data.ID); err != nil || !strings.Contains(first.Data.Note, "next turn") {
		t.Fatal("missing generated ID or busy timing", err)
	}
	args["message_id"] = first.Data.ID
	var retry Output[MessageReceipt]
	call("send_message", args, &retry)
	if retry.Data.ID != first.Data.ID || calls != 1 {
		t.Fatal("retry submitted another message")
	}
	var reply Output[MessageReceipt]
	replyArgs := map[string]any{"actor": to.ID, "capability": toKey, "message_id": first.Data.ID, "body": "reviewed"}
	call("reply_message", replyArgs, &reply)
	if reply.Data.ID == "" || reply.Data.ID == first.Data.ID || !strings.Contains(reply.Data.Note, "next delivery hook") {
		t.Fatal("reply lost generated identity or hook timing")
	}
	var repeated Output[MessageReceipt]
	call("reply_message", replyArgs, &repeated)
	if repeated.Data.ID != reply.Data.ID {
		t.Fatal("reply retry lost its deterministic identity")
	}
	var sent Output[MessagePage]
	call("list_messages", map[string]any{"actor": from.ID, "capability": key, "sent": true}, &sent)
	if len(sent.Data.Messages) != 1 || sent.Data.Messages[0].ID != first.Data.ID || sent.Data.Messages[0].AcknowledgedAt == "" || sent.Data.Messages[0].Status != bus.StatusSubmitted || !sent.Data.Messages[0].BodyOmitted {
		t.Fatal("sent receipts lost direction/status/ack or repeated the body")
	}
}

func TestReceiptNotesNeverPromiseRefusedOrUnconfirmedDelivery(t *testing.T) {
	queued, err := messageReceipt(bus.Message{Status: bus.StatusApprovalBlocked, Receipt: &bus.Receipt{Error: "queued until the permission prompt is answered"}}, nil)
	if err != nil || queued.Note != "Queued until the target's permission prompt is answered." {
		t.Fatal("a queued approval message was described as refused")
	}
	acknowledged, err := messageReceipt(bus.Message{Status: bus.StatusPendingActivity, AcknowledgedAt: "2026-11-01T00:00:00Z"}, nil)
	if err != nil || !acknowledged.Acknowledged || acknowledged.Note != "" {
		t.Fatal("an acknowledged message was still described as deferred")
	}
	for _, test := range []struct {
		status bus.DeliveryStatus
		timing bus.DeliveryTiming
		want   string
	}{
		{bus.StatusApprovalBlocked, "", "Not sent"},
		{bus.StatusPendingActivity, "", "next delivery hook"},
		{bus.StatusTransportWritten, bus.DeliveryBetweenTools, "between tool calls"},
		{bus.StatusSubmitted, bus.DeliveryNextTurn, "next turn"},
		{bus.StatusWakeWritten, bus.DeliveryNotification, "native notification"},
		{bus.StatusSubmitted, bus.DeliveryNextHook, "next hook"},
		{bus.StatusNotSent, bus.DeliveryNextTurn, ""},
		{bus.StatusUnknownTimeout, bus.DeliveryNextTurn, ""},
		{bus.StatusUnknownWrite, bus.DeliveryBetweenTools, ""},
	} {
		receipt, err := messageReceipt(bus.Message{Status: test.status, BusyDelivery: test.timing}, nil)
		if err != nil || (test.want == "" && receipt.Note != "") || (test.want != "" && !strings.Contains(receipt.Note, test.want)) {
			t.Fatalf("status=%s timing=%s: note=%q want=%q", test.status, test.timing, receipt.Note, test.want)
		}
	}
}
