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

func TestMCPGeneratedSendIDReplyCorrelationAndSentReceipts(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, t.TempDir())
	}
	service := newTestService(t)
	store := service.Store
	from, key, to, toKey := registerPair(t, store, "cursor", "00000000-0000-4000-8000-000000000011", "codex", "00000000-0000-4000-8000-000000000012")
	if err := store.Update(to.ID, bus.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	calls := 0
	service.Deliver = func(context.Context, bus.Message, bus.Session) bus.Receipt {
		calls++
		return bus.NewReceipt(bus.StatusSubmitted)
	}
	client, ctx := testClient(t, service)
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
	call("list_messages", map[string]any{"actor": from.ID, "capability": key, "sent": true, "to": to.ID, "preview": true}, &sent)
	if len(sent.Data.Messages) != 1 || sent.Data.Messages[0].Preview != "review" || sent.Data.Messages[0].Body != "" || !sent.Data.Messages[0].BodyOmitted {
		t.Fatal("sent preview/filter arguments did not reach the response view")
	}
	call("list_messages", map[string]any{"actor": from.ID, "capability": key, "sent": true, "to": from.ID, "preview": true}, &sent)
	if len(sent.Data.Messages) != 0 {
		t.Fatal("MCP recipient filter returned another recipient")
	}
}

func TestReceiptNotesNeverPromiseRefusedOrUnconfirmedDelivery(t *testing.T) {
	queued, err := messageReceipt(bus.Message{Status: bus.StatusApprovalBlocked, Held: true}, nil)
	if err != nil || queued.Note != "Queued until the target's permission prompt is answered." {
		t.Fatal("a queued approval message was described as refused")
	}
	acknowledged, err := messageReceipt(bus.Message{Status: bus.StatusPendingActivity, AcknowledgedAt: "2026-11-01T00:00:00Z"}, nil)
	if err != nil || !acknowledged.Acknowledged || acknowledged.Note != "" {
		t.Fatal("an acknowledged message was still described as deferred")
	}
	for _, test := range []struct {
		status bus.DeliveryStatus
		timing model.DeliveryTiming
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
