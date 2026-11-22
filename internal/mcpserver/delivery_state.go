package mcpserver

import (
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

// DeliveryState is one plain word for how far a message got. It is computed
// from the stored status and acknowledgement when an answer is built. It is
// never stored, so the stored delivery status and the state format do not change.
type DeliveryState string

const (
	// StateQueued means the message waits to be sent.
	StateQueued DeliveryState = "queued"
	// StateDelivered means the receiver's client has the message; nobody confirmed reading it.
	StateDelivered DeliveryState = "delivered"
	// StateRead means the receiver confirmed it with ack_message or reply_message.
	StateRead DeliveryState = "read"
	// StateFailed means the message was refused or could not be sent.
	StateFailed DeliveryState = "failed"
	// StateUnknown means the message may or may not have arrived.
	StateUnknown DeliveryState = "unknown"
)

// deliveryState names the stored status of a message. An empty status with no
// acknowledgement gives an empty state, so a call that failed before it made a
// message shows no state.
func deliveryState(msg model.Message) DeliveryState {
	if msg.AcknowledgedAt != "" {
		return StateRead
	}
	if msg.IsQueuedForApproval() {
		return StateQueued
	}
	switch msg.Status {
	case "":
		return ""
	case model.StatusAttempting, model.StatusPendingActivity, model.StatusWakeWritten:
		return StateQueued
	case model.StatusSubmitted:
		// A plugin inbox stores the message; the plugin reads it later.
		if strings.HasPrefix(msg.To, "plugin:") {
			return StateQueued
		}
		return StateDelivered
	case model.StatusTransportWritten, model.StatusContextEmitted:
		return StateDelivered
	case model.StatusNotSent, model.StatusApprovalBlocked, model.StatusCommandFailed:
		return StateFailed
	case model.StatusWakeUncertain, model.StatusUnknownTimeout, model.StatusUnknownWrite, model.StatusQueuedForApproval:
		return StateUnknown
	}
	return StateUnknown
}

// DeliveryReceipt is the result of send_message, reply_message and ack_message.
// It adds the state, and for a send or reply the receiver, to the stored receipt.
type DeliveryReceipt struct {
	MessageReceipt
	// State is one plain word: queued, delivered, read, failed or unknown.
	State DeliveryState `json:"state,omitempty"`
	// To and ToName show who the message went to; ToName is the current name.
	To     string `json:"to,omitempty"`
	ToName string `json:"to_name,omitempty"`
	// ParentAcknowledged is set on a reply: it also acknowledged the message it answers.
	ParentAcknowledged bool `json:"parent_acknowledged,omitempty"`
}

// DeliveryView is a message in a list result. For a message the caller sent it
// adds the state and the receiver's current name.
type DeliveryView struct {
	MessageView
	State  DeliveryState `json:"state,omitempty"`
	ToName string        `json:"to_name,omitempty"`
}

// agentNames maps the ID of each listed agent to its current name. It is a
// best-effort view: if discovery fails, or an agent has no name or is gone,
// the name is simply missing.
func agentNames(service Bus, args toolArgs) map[string]string {
	rows, _, err := service.DiscoverWithWarnings(args.Actor, args.Capability)
	if err != nil {
		return nil
	}
	names := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.Name != "" {
			names[row.ID] = row.Name
		}
	}
	return names
}

// ackReceipt is the result of ack_message: the message is read.
func ackReceipt(message model.Message, err error) (DeliveryReceipt, error) {
	receipt, err := messageReceipt(message, err)
	return DeliveryReceipt{MessageReceipt: receipt, State: deliveryState(message)}, err
}

// sentReceipt builds the result of send_message. It adds the resolved receiver
// (ID and current name), so a wrong address is visible at once.
func sentReceipt(service Bus, args toolArgs) func(model.Message, error) (DeliveryReceipt, error) {
	return func(message model.Message, err error) (DeliveryReceipt, error) {
		receipt, err := ackReceipt(message, err)
		if message.To != "" {
			receipt.To = message.To
			receipt.ToName = agentNames(service, args)[message.To]
		}
		return receipt, err
	}
}

// replyReceipt builds the result of reply_message. A reply acknowledges the
// message it answers; the flag is set only when the call returned without error.
func replyReceipt(service Bus, args toolArgs) func(model.Message, error) (DeliveryReceipt, error) {
	send := sentReceipt(service, args)
	return func(message model.Message, err error) (DeliveryReceipt, error) {
		receipt, err := send(message, err)
		receipt.ParentAcknowledged = err == nil && message.ID != ""
		return receipt, err
	}
}

// deliveryView adds the state and the receiver's name, but only for a message
// the actor sent; names maps receiver IDs to current names and may be nil.
func deliveryView(view MessageView, msg model.Message, actor string, names map[string]string) DeliveryView {
	if msg.From != actor {
		return DeliveryView{MessageView: view}
	}
	return DeliveryView{MessageView: view, State: deliveryState(msg), ToName: names[msg.To]}
}

// sentNames returns the names for the receivers of the messages the caller
// sent, or nil when the page has none (no discovery call is made then).
func sentNames(service Bus, args toolArgs, messages []model.Message) map[string]string {
	for _, msg := range messages {
		if msg.From == args.Actor {
			return agentNames(service, args)
		}
	}
	return nil
}
