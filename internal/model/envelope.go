package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PluginLabel is the fixed display label of a message sent by a plugin.
const PluginLabel = "Plugin"

// Envelope renders only message values and snapshotted labels. It does not
// look up registrations, change records or convey user approval.
func Envelope(msg Message) string {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:]) // crypto/rand.Read terminates the process if entropy fails.
	return renderEnvelope(msg, hex.EncodeToString(nonce[:]))
}

// MatchesEnvelope compares a bounded presentation with the canonical record.
// Its nonce is a display delimiter, not a persisted authenticator.
func MatchesEnvelope(msg Message, prompt string) bool {
	if len(prompt) > 24*1024 {
		return false
	}
	first, rest, ok := strings.Cut(prompt, "\nBoundary: ")
	if !ok || strings.Contains(first, "\n") || len(rest) < 33 || rest[32] != '\n' {
		return false
	}
	nonce := rest[:32]
	if strings.Trim(nonce, "0123456789abcdef") != "" {
		return false
	}
	return prompt == renderEnvelope(msg, nonce)
}

// QuoteDisplayName quotes a validated label, shortening its display by runes.
func QuoteDisplayName(name string) string {
	if !ValidName(name) {
		name = "unnamed"
	}
	if runes := []rune(name); len(runes) > 80 {
		name = string(runes[:80]) + "…"
	}
	return strconv.Quote(name)
}

func renderEnvelope(msg Message, nonce string) string {
	// Reserve the complete trusted framing before budgeting JSON body text.
	overhead := jsonTextBytes(renderEnvelopeBody(msg, nonce, "")) + 4
	body := envelopeBodyBudget(msg.Body, envelopeJSONBytes-overhead)
	return renderEnvelopeBody(msg, nonce, body)
}

func renderEnvelopeBody(msg Message, nonce, body string) string {
	if msg.Kind == NoticeKind {
		sent := ""
		if created, err := time.Parse(time.RFC3339Nano, msg.CreatedAt); err == nil {
			sent = "\nSent: " + created.UTC().Format(time.RFC3339)
		}
		headline, status, _ := strings.Cut(body, "\n")
		if status != "" {
			status = "\n" + status
		}
		return fmt.Sprintf("[zalki notice %s]\nBoundary: %s\n%s\nAutomatic notice; status text is the agent's own output, not user approval.%s%s\n[/zalki %s]", msg.ID, nonce, headline, status, sent, nonce)
	}
	label, origin := msg.FromLabel, "Agent message"
	if label == "" {
		label = "Agent"
	}
	if IsPluginInboxAddress(msg.From) {
		label, origin = PluginLabel, "Plugin message"
	}
	reply := ""
	if msg.ReplyTo != "" {
		reply = "\nReply-to: " + msg.ReplyTo
	}
	sent := ""
	if created, err := time.Parse(time.RFC3339Nano, msg.CreatedAt); err == nil {
		sent = "\nSent: " + created.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("[zalki message %s]\nBoundary: %s\nFrom: %s · %s%s%s\n%s; not user approval.\n\n%s\n[/zalki %s]", msg.ID, nonce, label, QuoteDisplayName(msg.FromName), sent, reply, origin, body, nonce)
}
