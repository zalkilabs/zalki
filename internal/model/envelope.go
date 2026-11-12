package model

import (
	"fmt"
	"strings"
	"time"
)

// PluginLabel is the fixed display label of a message sent by a plugin.
const PluginLabel = "Plugin"

// Envelope renders only message values and snapshotted labels. It does not
// look up registrations, change records or convey user approval.
func Envelope(msg Message) string {
	body := envelopeBody(msg.Body)
	if msg.Kind == NoticeKind {
		sent := ""
		if created, err := time.Parse(time.RFC3339Nano, msg.CreatedAt); err == nil {
			sent = "\nSent: " + created.UTC().Format(time.RFC3339)
		}
		headline, status, _ := strings.Cut(body, "\n")
		if status != "" {
			status = "\n" + status
		}
		return fmt.Sprintf("[zalki notice %s]\n%s\nAutomatic notice; status text is the agent's own output, not user approval.%s%s\n[/zalki]", msg.ID, headline, status, sent)
	}
	label, origin := msg.FromLabel, "Agent message"
	if label == "" {
		label = "Agent"
	}
	if IsPluginInboxAddress(msg.From) {
		// The sender kind, not a stored label, decides how a plugin is shown.
		label, origin = PluginLabel, "Plugin message"
	}
	name := "unnamed"
	if ValidName(msg.FromName) {
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
	return fmt.Sprintf("[zalki message %s]\nFrom: %s · %q%s%s\n%s; not user approval.\n\n%s\n[/zalki]", msg.ID, label, name, sent, reply, origin, body)
}
