package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestDeliveryStateNamesEveryStoredStatus(t *testing.T) {
	const at = "2026-11-22T00:00:00Z"
	tests := []struct {
		name string
		msg  model.Message
		want DeliveryState
	}{
		{"attempting", model.Message{Status: model.StatusAttempting}, StateQueued},
		{"next activity", model.Message{Status: model.StatusPendingActivity}, StateQueued},
		{"wake hint written", model.Message{Status: model.StatusWakeWritten}, StateQueued},
		{"held for approval", model.Message{Status: model.StatusApprovalBlocked, Held: true}, StateQueued},
		{"plugin inbox", model.Message{Status: model.StatusSubmitted, To: "plugin:" + model.NewUUID()}, StateQueued},
		{"submitted", model.Message{Status: model.StatusSubmitted, To: "codex:" + model.NewUUID()}, StateDelivered},
		{"transport written", model.Message{Status: model.StatusTransportWritten}, StateDelivered},
		{"hook context", model.Message{Status: model.StatusContextEmitted}, StateDelivered},
		{"not sent", model.Message{Status: model.StatusNotSent}, StateFailed},
		{"command failed", model.Message{Status: model.StatusCommandFailed}, StateFailed},
		{"final approval refusal", model.Message{Status: model.StatusApprovalBlocked}, StateFailed},
		{"wake hint uncertain", model.Message{Status: model.StatusWakeUncertain}, stateUnknown},
		{"unknown after timeout", model.Message{Status: model.StatusUnknownTimeout}, stateUnknown},
		{"unknown after write", model.Message{Status: model.StatusUnknownWrite}, stateUnknown},
		{"acknowledged delivered", model.Message{Status: model.StatusTransportWritten, AcknowledgedAt: at}, StateRead},
		{"acknowledged after uncertain delivery", model.Message{Status: model.StatusUnknownWrite, AcknowledgedAt: at}, StateRead},
		{"acknowledged while queued", model.Message{Status: model.StatusPendingActivity, AcknowledgedAt: at}, StateRead},
		{"no status", model.Message{}, ""},
	}
	for _, test := range tests {
		if got := deliveryState(test.msg); got != test.want {
			t.Errorf("%s: state %q, want %q", test.name, got, test.want)
		}
	}
}

// namedBus answers discovery with a fixed agent list; the other methods come from fakeBus.
type namedBus struct {
	fakeBus
	sent, replied model.Message
	agents        []bus.AgentInfo
	discoverErr   error
	discovered    int
}

func (n *namedBus) Send(context.Context, bus.SendRequest) (model.Message, error) {
	return n.sent, n.err
}

func (n *namedBus) Reply(context.Context, bus.ReplyRequest) (model.Message, error) {
	return n.replied, n.err
}

func (n *namedBus) DiscoverWithWarnings(string, string) ([]bus.AgentInfo, int, error) {
	n.discovered++
	return n.agents, 0, n.discoverErr
}

func namedFixture() (*namedBus, string) {
	to := "claude:" + model.NewUUID()
	return &namedBus{
		sent:    model.Message{ID: model.NewUUID(), To: to, Status: model.StatusTransportWritten},
		replied: model.Message{ID: model.NewUUID(), To: to, Status: model.StatusSubmitted},
		agents:  []bus.AgentInfo{{ID: to, Name: "lead"}, {ID: "claude:" + model.NewUUID(), Name: "other"}, {ID: "claude:" + model.NewUUID()}},
	}, to
}

func TestSendReceiptShowsReceiverAndState(t *testing.T) {
	service, to := namedFixture()
	receipt, err := sentReceipt(service, toolArgs{Actor: "a", Capability: "k"})(service.sent, nil)
	if err != nil || receipt.To != to || receipt.ToName != "lead" || receipt.State != StateDelivered || receipt.Acknowledged || receipt.ParentAcknowledged {
		t.Fatalf("send receipt: %+v %v", receipt, err)
	}
}

func TestReplyReceiptSaysTheParentWasAcknowledged(t *testing.T) {
	service, _ := namedFixture()
	reply := replyReceipt(service, toolArgs{Actor: "a", Capability: "k"})
	receipt, err := reply(service.replied, nil)
	if err != nil || receipt.ToName != "lead" || receipt.State != StateDelivered || !receipt.ParentAcknowledged {
		t.Fatalf("reply receipt: %+v %v", receipt, err)
	}
	receipt, err = reply(service.replied, errors.New("delivery failed"))
	if err == nil || receipt.ParentAcknowledged {
		t.Fatalf("a failed reply must not claim the parent was acknowledged: %+v", receipt)
	}
}

func TestAckReceiptIsReadAndNamesNoReceiver(t *testing.T) {
	acknowledged, err := ackReceipt(model.Message{ID: model.NewUUID(), Status: model.StatusTransportWritten, AcknowledgedAt: "2026-11-22T00:00:00Z"}, nil, codexNotes)
	if err != nil || acknowledged.State != StateRead || acknowledged.To != "" || acknowledged.ToName != "" || !acknowledged.Acknowledged {
		t.Fatalf("ack receipt: %+v %v", acknowledged, err)
	}
}

func TestReceiverNameIsLeftOutWhenUnknown(t *testing.T) {
	to := "claude:" + model.NewUUID()
	msg := model.Message{ID: model.NewUUID(), To: to, Status: model.StatusTransportWritten}
	retired := &namedBus{agents: []bus.AgentInfo{{ID: "claude:" + model.NewUUID(), Name: "someone else"}}}
	receipt, err := sentReceipt(retired, toolArgs{})(msg, nil)
	if err != nil || receipt.To != to || receipt.ToName != "" {
		t.Fatalf("retired receiver: %+v %v", receipt, err)
	}
	failing := &namedBus{discoverErr: errors.New("discovery down")}
	receipt, err = sentReceipt(failing, toolArgs{})(msg, nil)
	if err != nil || receipt.To != to || receipt.ToName != "" || receipt.State != StateDelivered {
		t.Fatalf("discovery failure must not fail the send: %+v %v", receipt, err)
	}
	refused, err := sentReceipt(failing, toolArgs{})(model.Message{}, errors.New("refused"))
	if err == nil || refused.To != "" || refused.State != "" || failing.discovered != 1 {
		t.Fatalf("a call that made no message needs no receiver or lookup: %+v %v", refused, err)
	}
}

func TestMessageViewsShowStateAndNameOnlyForSentMessages(t *testing.T) {
	me, other := "claude:"+model.NewUUID(), "claude:"+model.NewUUID()
	sent := model.Message{ID: model.NewUUID(), From: me, To: other, Status: model.StatusTransportWritten}
	received := model.Message{ID: model.NewUUID(), From: other, To: me, Status: model.StatusTransportWritten}
	names := map[string]string{other: "lead", me: "worker"}
	views := messageViews([]model.Message{sent, received}, me, names, codexNotes)
	if views[0].State != StateDelivered || views[0].ToName != "lead" {
		t.Fatalf("sent view: %+v", views[0])
	}
	if views[1].State != "" || views[1].ToName != "" {
		t.Fatalf("received view must not claim a sender-side state or name: %+v", views[1])
	}
	sent.AcknowledgedAt = "2026-11-22T00:00:00Z"
	if view := messageViews([]model.Message{sent}, me, nil, codexNotes)[0]; view.State != StateRead || view.ToName != "" {
		t.Fatalf("read view without names: %+v", view)
	}
}

func TestSentNamesLooksUpOnlyWhenThePageHasSentMessages(t *testing.T) {
	me, other := "claude:"+model.NewUUID(), "claude:"+model.NewUUID()
	service := &namedBus{agents: []bus.AgentInfo{{ID: other, Name: "lead"}}}
	args := toolArgs{Actor: me, Capability: "k"}
	if names := sentNames(service, args, []model.Message{{From: other, To: me}}); names != nil || service.discovered != 0 {
		t.Fatalf("an inbox page needs no lookup: %v %d", names, service.discovered)
	}
	if names := sentNames(service, args, []model.Message{{From: me, To: other}}); names[other] != "lead" || service.discovered != 1 {
		t.Fatalf("a sent page needs one lookup: %v %d", names, service.discovered)
	}
}

// The state follows the real transitions: a held message is queued, and after
// the approval queue releases it, it becomes delivered and then read.
func TestDeliveryStateFollowsAHeldMessageThroughRelease(t *testing.T) {
	msg := model.Message{ID: model.NewUUID(), From: "claude:" + model.NewUUID(), To: "claude:" + model.NewUUID(), Status: model.StatusAttempting}
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name string
		do   func() error
		want DeliveryState
	}{
		{"held", func() error { return nil }, StateQueued},
		{"released", func() error { return msg.ReleaseHeld(model.StatusAttempting) }, StateQueued},
		{"written", func() error { return msg.ApplyReceipt(model.NewReceipt(model.StatusTransportWritten)) }, StateDelivered},
		{"acknowledged", func() error { msg.AcknowledgedAt = model.Timestamp(); return nil }, StateRead},
	}
	for _, step := range steps {
		if err := step.do(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := deliveryState(msg); got != step.want {
			t.Fatalf("%s: state %q, want %q", step.name, got, step.want)
		}
	}
	refused := model.Message{Status: model.StatusAttempting}
	if err := refused.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := refused.SettleNotSent(model.CodeUnavailable, "gone"); err != nil || deliveryState(refused) != StateFailed {
		t.Fatalf("a refused held message is failed: %v %q", err, deliveryState(refused))
	}
}

func callTool(ctx context.Context, t *testing.T, client *mcp.ClientSession, name string, args map[string]any, out any) {
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

func wantState(t *testing.T, label string, got, want DeliveryState) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: state %q, want %q", label, got, want)
	}
}

// onlyState returns the state of the one message in a page.
func onlyState(t *testing.T, page MessagePage) DeliveryState {
	t.Helper()
	if len(page.Messages) != 1 {
		t.Fatalf("want one message, got %d", len(page.Messages))
	}
	return page.Messages[0].State
}

// The whole path through the real bus: delivered after the send, read after the
// reply, and the reply says it acknowledged the parent.
func TestSendReplyAndSentHistoryShowTheState(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	service := newTestService(t)
	from, key, to, toKey := registerPair(t, service.Store, "cursor", "00000000-0000-4000-8000-000000000031", "codex", "00000000-0000-4000-8000-000000000032")
	service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		return model.NewReceipt(model.StatusSubmitted)
	}
	client, ctx := testClient(t, service)
	var sent, reply Output[DeliveryReceipt]
	var page Output[MessagePage]
	callTool(ctx, t, client, "send_message", map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "body": "review"}, &sent)
	wantState(t, "send", sent.Data.State, StateDelivered)
	if sent.Data.To != to.ID {
		t.Fatalf("the send result must name the receiver: %+v", sent.Data)
	}
	sentList := map[string]any{"actor": from.ID, "capability": key, "sent": true}
	callTool(ctx, t, client, "list_messages", sentList, &page)
	wantState(t, "sent history before the reply", onlyState(t, page.Data), StateDelivered)
	callTool(ctx, t, client, "reply_message", map[string]any{"actor": to.ID, "capability": toKey, "message_id": sent.Data.ID, "body": "done"}, &reply)
	if !reply.Data.ParentAcknowledged || reply.Data.To != from.ID {
		t.Fatalf("a reply says it acknowledged the parent: %+v", reply.Data)
	}
	wantState(t, "reply to a hook-pending receiver", reply.Data.State, StateQueued)
	callTool(ctx, t, client, "list_messages", sentList, &page)
	wantState(t, "sent history after the reply", onlyState(t, page.Data), StateRead)
	// The receiver's own history shows no sender-side state for what it received.
	callTool(ctx, t, client, "list_messages", map[string]any{"actor": to.ID, "capability": toKey, "history": true}, &page)
	for _, view := range page.Data.Messages {
		if view.From == from.ID {
			wantState(t, "received message", view.State, "")
		}
	}
}

func TestInstructionsExplainTheStatesAndAckMessage(t *testing.T) {
	client, _ := fakeClient(t, &fakeBus{})
	instructions := client.InitializeResult().Instructions
	for _, text := range []string{
		"no ACK-only messages",
		"call ack_message (it sends nothing)",
		"reply_message also acknowledges the message you answer",
		"queued (waiting), delivered (handed to the receiver's client, not confirmed read), read",
		"acknowledged:false only means not read yet",
	} {
		if !strings.Contains(instructions, text) {
			t.Fatalf("instructions miss %q", text)
		}
	}
}

// isQueuedReplyReceipt reports whether a reply result to a Cursor receiver has
// exactly the keys of a compact receipt: the stored receipt, the state, the
// receiver and the parent acknowledgement.
func isQueuedReplyReceipt(data map[string]any, receiver string) bool {
	return len(data) == 7 && data["state"] == string(StateQueued) && data["parent_acknowledged"] == true && data["to"] == receiver
}

// The five state words are part of the tool results and their schema: a
// constant may become private, its word may not change.
func TestDeliveryStateWordsDoNotChange(t *testing.T) {
	for state, word := range map[DeliveryState]string{StateQueued: "queued", StateDelivered: "delivered", StateRead: "read", StateFailed: "failed", stateUnknown: "unknown"} {
		if string(state) != word {
			t.Fatalf("state %q, want %q", state, word)
		}
	}
}
