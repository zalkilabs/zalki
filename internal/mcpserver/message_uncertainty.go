package mcpserver

import (
	"fmt"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

const (
	uncertainSenderText = "Message %s may have been delivered; ask the receiver to ACK it, and do not resend it without checking."
	uncertainSinceText  = "Delivery unconfirmed since %s; message %s may have been delivered, so ask the receiver to ACK it and do not resend it without checking."
)

// uncertainNotes gives the sentences a client declares for an unconfirmed
// delivery to it (see bus.Service.UncertainDelivery). Empty means none.
type uncertainNotes func(kind string) (receiver, acknowledged string)

// uncertaintyText derives display text only. It never changes a delivery record
// or treats a later ACK as confirmation of the earlier native attempt.
func uncertaintyText(msg model.Message, actor string, history bool, notes uncertainNotes) string {
	receiver, acknowledged, ok := uncertainSentences(msg, notes)
	if !ok {
		return ""
	}
	if msg.AcknowledgedAt != "" {
		return fmt.Sprintf(acknowledged, msg.ID)
	}
	text := fmt.Sprintf(uncertainSenderText, msg.ID)
	if history && actor == msg.To {
		text = fmt.Sprintf(receiver, msg.ID)
	}
	if !history || msg.Receipt == nil || !validUncertaintyTime(msg.Receipt.At) {
		return text
	}
	since := fmt.Sprintf(uncertainSinceText, localTime(msg.Receipt.At), msg.ID)
	if actor == msg.To {
		return text + "\n\n" + since
	}
	return since
}

// uncertainSentences says whether msg is an unconfirmed delivery to a client with
// its own wording, and gives that wording.
func uncertainSentences(msg model.Message, notes uncertainNotes) (receiver, acknowledged string, ok bool) {
	if (msg.Status != model.StatusUnknownWrite && msg.Status != model.StatusUnknownTimeout) ||
		model.ValidateSessionAddress(msg.To) != nil || !model.IsCanonicalUUID(msg.ID) || notes == nil {
		return "", "", false
	}
	kind, _, _ := strings.Cut(msg.To, ":")
	receiver, acknowledged = notes(kind)
	return receiver, acknowledged, receiver != ""
}

func validUncertaintyTime(value string) bool {
	// Bound the displayed spelling too: Parse accepts arbitrarily long fractions.
	if len(value) < 20 || len(value) > 35 {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}
