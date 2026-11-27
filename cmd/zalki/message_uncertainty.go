package main

import (
	"fmt"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

const uncertainSenderText = "Message %s may have been delivered; ask the receiver to ACK it, and do not resend it without checking."

// sendOutput changes only a returned copy. In particular, the receipt pointer
// may still belong to the service's original message and must not be edited.
func sendOutput(message model.Message, registry *agent.Registry) model.Message {
	acknowledged, ok := uncertainAcknowledgedText(message, registry)
	if !ok {
		return message
	}
	receipt := model.Receipt{Status: message.Status}
	if message.Receipt != nil {
		receipt = *message.Receipt
	}
	receipt.Error = fmt.Sprintf(uncertainSenderText, message.ID)
	if message.AcknowledgedAt != "" {
		receipt.Error = fmt.Sprintf(acknowledged, message.ID)
	}
	message.Receipt = &receipt
	return message
}

// uncertainAcknowledgedText says whether the message is an unconfirmed delivery
// to a client that has its own wording, and gives that client's sentence for an
// ACKed message. The client declares both in its descriptor.
func uncertainAcknowledgedText(message model.Message, registry *agent.Registry) (string, bool) {
	if (message.Status != model.StatusUnknownWrite && message.Status != model.StatusUnknownTimeout) ||
		model.ValidateSessionAddress(message.To) != nil || !model.IsCanonicalUUID(message.ID) {
		return "", false
	}
	kind, _, _ := strings.Cut(message.To, ":")
	adapter, err := registry.Lookup(kind)
	if err != nil {
		return "", false
	}
	descriptor := adapter.Descriptor()
	return descriptor.UncertainAcknowledged, descriptor.UncertainReceiver != ""
}
