package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSendUncertainJSONGuidance(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusUnknownWrite, model.StatusUnknownTimeout} {
		t.Run(string(status), func(t *testing.T) { checkSendUncertainJSON(t, status) })
	}
}

func checkSendUncertainJSON(t *testing.T, status model.DeliveryStatus) {
	t.Helper()
	const id = "00000000-0000-4000-8000-000000000123"
	msg := model.Message{ID: id, To: "codex:00000000-0000-4000-8000-000000000124", Status: status, Receipt: &model.Receipt{Status: status, Code: model.CodeUnavailable, Error: "private-native-marker", At: "2026-11-13T22:00:00Z", Output: "controlled original output"}}
	before, _ := json.Marshal(msg)
	out := sendOutput(msg, uncertainRegistry(t))
	var buffer bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buffer)
	if err := encodeOutput(cmd, out); err != nil {
		t.Fatal(err)
	}
	var decoded model.Message
	if err := json.Unmarshal(buffer.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	want := "Message " + id + " may have been delivered; ask the receiver to ACK it, and do not resend it without checking."
	if decoded.Receipt == nil || decoded.Receipt.Error != want {
		t.Fatal("CLI uncertainty guidance missing")
	}
	if decoded.Status != status || decoded.ID != id || decoded.Receipt.Code != msg.Receipt.Code || decoded.Receipt.At != msg.Receipt.At || decoded.Receipt.Output != msg.Receipt.Output {
		t.Fatal("uncertain status or original ID changed")
	}
	after, _ := json.Marshal(msg)
	if string(before) != string(after) || out.Receipt == msg.Receipt {
		t.Fatal("display formatting mutated original receipt")
	}
	msg.AcknowledgedAt = "2026-11-13T23:00:00Z"
	if got := sendOutput(msg, uncertainRegistry(t)); got.Receipt.Error != "The receiver ACKed message "+id+", but Codex did not confirm the earlier delivery attempt." {
		t.Fatal("ACKed uncertainty still asks for ACK")
	}
}

func TestSendUncertainExcludesOtherStates(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusApprovalBlocked, model.StatusSubmitted, model.StatusNotSent} {
		msg := model.Message{Status: status, Receipt: &model.Receipt{Error: "unchanged"}}
		if got := sendOutput(msg, uncertainRegistry(t)); got.Receipt != msg.Receipt || got.Receipt.Error != "unchanged" {
			t.Fatal("CLI changed another delivery outcome")
		}
	}
	msg := model.Message{ID: "unsafe\nmarker", Status: model.StatusUnknownWrite, To: "codex:invalid"}
	if got := sendOutput(msg, uncertainRegistry(t)); got.Receipt != nil {
		t.Fatal("CLI formatted an invalid identity")
	}
	if strings.Contains(uncertainSenderText, "blindly") {
		t.Fatal("CLI copy lost approved plain English")
	}
}

func TestSendCommandProjectsStoredUncertainty(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusUnknownWrite, model.StatusUnknownTimeout} {
		t.Run(string(status), func(t *testing.T) { checkStoredUncertainSend(t, status) })
	}
}

func checkStoredUncertainSend(t *testing.T, status model.DeliveryStatus) {
	t.Helper()
	home := configTestHome(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	opts := newCommandOptions(home)
	opts.root = newRootCommand(home)
	service, err := opts.service()
	if err != nil {
		t.Fatal(err)
	}
	from, key, err := service.Store.Register("cursor", "00000000-0000-4000-8000-000000000141", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _, err := service.Store.Register("codex", "00000000-0000-4000-8000-000000000142", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: "00000000-0000-4000-8000-000000000143", Body: "stored uncertain send"}
	if _, _, _, err := service.Store.ReserveMessage(request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.Finish(request.MessageID, model.Receipt{Status: status, Error: "private-native-marker"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_CAPABILITY", key)
	raw, _ := json.Marshal(map[string]string{"actor": from.ID, "to": to.ID, "message_id": request.MessageID, "body": request.Body})
	cmd := newSendCommand(opts)
	cmd.SetIn(bytes.NewReader(raw))
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal("CLI changed unknown-result exit policy", err)
	}
	var message model.Message
	if json.Unmarshal(out.Bytes(), &message) != nil || message.Status != status || message.Receipt == nil || !strings.Contains(message.Receipt.Error, "do not resend it without checking.") {
		t.Fatal("CLI send wiring omitted uncertainty guidance")
	}
}

func TestSendUncertainWithoutReceiptKeepsKnownStatus(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusUnknownWrite, model.StatusUnknownTimeout} {
		msg := model.Message{ID: "00000000-0000-4000-8000-000000000123", To: "codex:00000000-0000-4000-8000-000000000124", Status: status}
		out := sendOutput(msg, uncertainRegistry(t))
		if out.Receipt == nil || out.Receipt.Status != status || out.Receipt.At != "" || !strings.Contains(out.Receipt.Error, "do not resend it without checking.") || msg.Receipt != nil {
			t.Fatal("missing receipt lost known uncertainty or invented a timestamp")
		}
	}
}

// uncertainFixture is a client with its own wording. Its sentences are the
// ones Codex declares today; another kind has none.
type uncertainFixture struct{ kind, acknowledged, receiver string }

func (a uncertainFixture) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: a.kind, Label: strings.ToUpper(a.kind[:1]) + a.kind[1:], UncertainReceiver: a.receiver, UncertainAcknowledged: a.acknowledged}
}
func (uncertainFixture) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn}
}
func (uncertainFixture) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}

func uncertainRegistry(t *testing.T) *agent.Registry {
	t.Helper()
	registry, err := agent.NewRegistry(
		uncertainFixture{kind: "codex", receiver: "receiver %s", acknowledged: "The receiver ACKed message %s, but Codex did not confirm the earlier delivery attempt."},
		uncertainFixture{kind: "plain"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// A client without its own wording keeps the receipt it has, for the same status.
func TestSendUncertainNeedsTheClientsOwnWording(t *testing.T) {
	msg := model.Message{ID: "00000000-0000-4000-8000-000000000123", To: "plain:00000000-0000-4000-8000-000000000124", Status: model.StatusUnknownWrite, Receipt: &model.Receipt{Error: "unchanged"}}
	if got := sendOutput(msg, uncertainRegistry(t)); got.Receipt != msg.Receipt {
		t.Fatal("a client without wording got Codex guidance")
	}
	// A valid message id with an invalid address is not formatted.
	for _, to := range []string{"codex:invalid", "codex", "codex:00000000-0000-4000-8000-000000000124:x", ""} {
		bad := model.Message{ID: msg.ID, To: to, Status: model.StatusUnknownWrite, Receipt: &model.Receipt{Error: "unchanged"}}
		if got := sendOutput(bad, uncertainRegistry(t)); got.Receipt != bad.Receipt {
			t.Fatalf("an invalid address %q was formatted", to)
		}
	}
	msg.To = "missing:00000000-0000-4000-8000-000000000124"
	if got := sendOutput(msg, uncertainRegistry(t)); got.Receipt != msg.Receipt {
		t.Fatal("an unknown client got guidance")
	}
}
