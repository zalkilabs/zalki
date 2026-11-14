package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

const (
	uncertainSenderText       = "Message %s may have been delivered; ask the receiver to ACK it, and do not resend it without checking."
	uncertainReceiverText     = "Message %s may already be in your Codex thread. If you can see it, ACK it; otherwise tell the sender delivery is still unconfirmed. Sending another copy may duplicate it."
	uncertainAcknowledgedText = "The receiver ACKed message %s, but Codex did not confirm the earlier delivery attempt."
	uncertainSinceText        = "Delivery unconfirmed since %s; message %s may have been delivered, so ask the receiver to ACK it and do not resend it without checking."
)

// uncertaintyText derives display text only. It never changes a delivery record
// or treats a later ACK as confirmation of the earlier native attempt.
func uncertaintyText(msg model.Message, actor string, history bool) string {
	if !isCodexUncertain(msg) {
		return ""
	}
	if msg.AcknowledgedAt != "" {
		return fmt.Sprintf(uncertainAcknowledgedText, msg.ID)
	}
	text := fmt.Sprintf(uncertainSenderText, msg.ID)
	if history && actor == msg.To {
		text = fmt.Sprintf(uncertainReceiverText, msg.ID)
	}
	if !history || msg.Receipt == nil || !validUncertaintyTime(msg.Receipt.At) {
		return text
	}
	since := fmt.Sprintf(uncertainSinceText, msg.Receipt.At, msg.ID)
	if actor == msg.To {
		return text + "\n\n" + since
	}
	return since
}

func isCodexUncertain(msg model.Message) bool {
	return (msg.Status == model.StatusUnknownWrite || msg.Status == model.StatusUnknownTimeout) &&
		strings.HasPrefix(msg.To, "codex:") && model.ValidateSessionAddress(msg.To) == nil && model.IsCanonicalUUID(msg.ID)
}

func validUncertaintyTime(value string) bool {
	// Bound the displayed spelling too: Parse accepts arbitrarily long fractions.
	if len(value) < 20 || len(value) > 35 {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}
