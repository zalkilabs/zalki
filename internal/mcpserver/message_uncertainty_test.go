package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

const uncertaintyID = "00000000-0000-4000-8000-000000000123"
const uncertaintyTarget = "codex:00000000-0000-4000-8000-000000000124"

func uncertainMessage(status model.DeliveryStatus) model.Message {
	return model.Message{ID: uncertaintyID, From: "cursor:00000000-0000-4000-8000-000000000125", To: uncertaintyTarget, Status: status,
		CreatedAt: "2020-01-01T00:00:00Z", Receipt: &model.Receipt{Status: status, Code: model.CodeUnavailable, Error: "private-native-marker"}}
}

func TestUncertainReceiptGuidance(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusUnknownWrite, model.StatusUnknownTimeout} {
		t.Run(string(status), func(t *testing.T) {
			msg := uncertainMessage(status)
			receipt, err := messageReceipt(msg, nil)
			want := "Message " + uncertaintyID + " may have been delivered; ask the receiver to ACK it, and do not resend it without checking."
			if err != nil || receipt.Error != want {
				t.Fatalf("%s guidance missing: %+v %v", status, receipt, err)
			}
			if receipt.ID != msg.ID || receipt.Status != status || receipt.Code != msg.Receipt.Code || receipt.Note != "" || receipt.Acknowledged {
				t.Fatal("uncertain status or original ID changed")
			}
			msg.AcknowledgedAt = "2026-11-13T23:00:00Z"
			receipt, err = messageReceipt(msg, nil)
			if err != nil || !receipt.Acknowledged || receipt.Error != "The receiver ACKed message "+uncertaintyID+", but Codex did not confirm the earlier delivery attempt." {
				t.Fatal("ACKed uncertainty still asks for ACK", receipt)
			}
		})
	}
}

func TestUncertainMessageHistoryGuidance(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusUnknownWrite, model.StatusUnknownTimeout} {
		msg := uncertainMessage(status)
		sender := messageViews([]model.Message{msg}, msg.From)[0]
		receiver := messageViews([]model.Message{msg}, msg.To)[0]
		want := "Message " + uncertaintyID + " may already be in your Codex thread. If you can see it, ACK it; otherwise tell the sender delivery is still unconfirmed. Sending another copy may duplicate it."
		if receiver.Error != want {
			t.Fatal("receiver guidance lost unconfirmed/duplication warning", receiver.Error)
		}
		receipt, _ := messageReceipt(msg, nil)
		if sender.Error != receipt.Error || receiver.Status != status || receiver.Code != msg.Receipt.Code {
			t.Fatal("uncertain history changed identity/status/code or sender copy")
		}
		msg.AcknowledgedAt = "2026-11-13T23:00:00Z"
		acknowledged := messageViews([]model.Message{msg}, msg.To)[0]
		if acknowledged.Error != "The receiver ACKed message "+uncertaintyID+", but Codex did not confirm the earlier delivery attempt." {
			t.Fatal("ACKed history still asks for ACK")
		}
	}
}

func TestUncertainMessageHistoryTime(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusUnknownWrite, model.StatusUnknownTimeout} {
		for _, at := range []string{"2026-11-13T23:00:00.123456789Z", "2026-11-14T01:00:00+02:00", "", "not-a-time", "2026-11-13T23:00:00Z\nprivate", "2026-11-13T23:00:00." + strings.Repeat("1", 1000) + "Z"} {
			checkUncertainTime(t, status, at)
		}
	}
}

func checkUncertainTime(t *testing.T, status model.DeliveryStatus, at string) {
	t.Helper()
	msg := uncertainMessage(status)
	msg.Receipt.At = at
	view := messageViews([]model.Message{msg}, msg.From)[0]
	if at == "" || strings.HasPrefix(at, "not-") || strings.Contains(at, "\n") || len(at) > 35 {
		if strings.Contains(view.Error, "since") || strings.Contains(view.Error, msg.CreatedAt) {
			t.Fatal("unconfirmed time came from outside receipt.at", view.Error)
		}
		return
	}
	want := fmt.Sprintf("Delivery unconfirmed since %s; message %s may have been delivered, so ask the receiver to ACK it and do not resend it without checking.", at, msg.ID)
	if view.Error != want {
		t.Fatal("validated receipt.at formatting changed", view.Error)
	}
	receiver := messageViews([]model.Message{msg}, msg.To)[0]
	if !strings.HasSuffix(receiver.Error, "\n\n"+want) || !strings.Contains(receiver.Error, "Sending another copy may duplicate it.") {
		t.Fatal("receiver time displaced receiver guidance")
	}
	msg.AcknowledgedAt = "2026-11-14T00:00:00Z"
	if strings.Contains(messageViews([]model.Message{msg}, msg.To)[0].Error, "since") {
		t.Fatal("ACKed history kept unconfirmed time")
	}
}

func TestUncertainProjectionExcludesOtherStates(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusApprovalBlocked, model.StatusSubmitted, model.StatusNotSent, model.StatusPendingActivity} {
		msg := uncertainMessage(status)
		msg.Held = true
		msg.Receipt.At = "2026-11-13T23:00:00Z"
		if uncertaintyText(msg, msg.To, true) != "" {
			t.Fatal("approval queue described as uncertain delivery")
		}
	}
	for _, to := range []string{"claude:00000000-0000-4000-8000-000000000124", "codex:unsafe\nmarker"} {
		msg := uncertainMessage(model.StatusUnknownWrite)
		msg.To = to
		if uncertaintyText(msg, msg.To, true) != "" {
			t.Fatal("non-Codex record received Codex guidance")
		}
	}
	msg := uncertainMessage(model.StatusUnknownWrite)
	msg.ID = "unsafe\nmarker"
	if uncertaintyText(msg, msg.To, true) != "" {
		t.Fatal("invalid message ID entered guidance")
	}
}

func TestUncertainHistoryProjectionDoesNotPersist(t *testing.T) {
	msg := uncertainMessage(model.StatusUnknownWrite)
	before, _ := json.Marshal(msg)
	page := messagePage(bus.MessageBatch{Messages: []model.Message{msg}}, true, msg.To)
	_, _ = messageReceipt(msg, nil)
	after, _ := json.Marshal(msg)
	if string(before) != string(after) {
		t.Fatal("display formatting mutated original receipt")
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "private-native-marker") || strings.Contains(string(encoded), "receipt") {
		t.Fatal("uncertain projection exposed native diagnostic or new structured receipt")
	}
}

func TestUncertainPageAccountsForGuidance(t *testing.T) {
	messages := make([]model.Message, 0, 100)
	for range 100 {
		messages = append(messages, uncertainMessage(model.StatusUnknownWrite))
	}
	page := messagePage(bus.MessageBatch{Messages: messages}, false, uncertaintyTarget)
	raw, _ := json.Marshal(page)
	if len(raw) > messagePageBytes || !page.More || len(page.Messages) == 0 {
		t.Fatal("uncertain guidance bypassed page byte accounting")
	}
}
