package main

import (
	"fmt"
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

const (
	uncertainSenderText       = "Message %s may have been delivered; ask the receiver to ACK it, and do not resend it without checking."
	uncertainAcknowledgedText = "The receiver ACKed message %s, but Codex did not confirm the earlier delivery attempt."
)

// sendOutput changes only a returned copy. In particular, the receipt pointer
// may still belong to the service's original message and must not be edited.
func sendOutput(message model.Message) model.Message {
	if (message.Status != model.StatusUnknownWrite && message.Status != model.StatusUnknownTimeout) ||
		!strings.HasPrefix(message.To, "codex:") || model.ValidateSessionAddress(message.To) != nil || !model.IsCanonicalUUID(message.ID) {
		return message
	}
	receipt := model.Receipt{Status: message.Status}
	if message.Receipt != nil {
		receipt = *message.Receipt
	}
	receipt.Error = fmt.Sprintf(uncertainSenderText, message.ID)
	if message.AcknowledgedAt != "" {
		receipt.Error = fmt.Sprintf(uncertainAcknowledgedText, message.ID)
	}
	message.Receipt = &receipt
	return message
}
