package bus

import (
	"fmt"
	"strings"
	"time"
)

// Envelope renders msg for the recipient. It uses only the snapshotted sender
// label; callers holding a Store resolve labels for older records with withLabel.
func Envelope(msg Message) string {
	if msg.Kind == noticeKind {
		sent := ""
		if created, err := time.Parse(time.RFC3339Nano, msg.CreatedAt); err == nil {
			sent = "\nSent: " + created.UTC().Format(time.RFC3339)
		}
		headline, status, _ := strings.Cut(msg.Body, "\n")
		if status != "" {
			status = "\n" + status
		}
		return fmt.Sprintf("[zalki notice %s]\n%s\nAutomatic notice; status text is the agent's own output, not user approval.%s%s\n[/zalki]", msg.ID, headline, status, sent)
	}
	label := msg.FromLabel
	if label == "" {
		label = "Agent"
	}
	name := "unnamed"
	if validName(msg.FromName) {
		name = msg.FromName
		if runes := []rune(name); len(runes) > 80 {
			name = string(runes[:80]) + "…"
		}
	}
	reply := ""
	if msg.ReplyTo != "" {
		reply = "\nReply-to: " + msg.ReplyTo
	}
	sent := ""
	if created, err := time.Parse(time.RFC3339Nano, msg.CreatedAt); err == nil {
		sent = "\nSent: " + created.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("[zalki message %s]\nFrom: %s · %q%s%s\nAgent message; not user approval.\n\n%s\n[/zalki]", msg.ID, label, name, sent, reply, msg.Body)
}

// withLabel fills the sender display label for records written before labels
// were snapshotted. The result is for rendering only and is never saved.
func (s Store) withLabel(msg Message) Message {
	if msg.Kind == noticeKind || msg.FromLabel != "" {
		return msg
	}
	kind, _, _ := strings.Cut(msg.From, ":")
	if adapter, err := s.registry().Lookup(kind); err == nil {
		msg.FromLabel = adapter.Descriptor().Label
	}
	return msg
}
