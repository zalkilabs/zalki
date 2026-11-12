// Envelope expectations are literal recipient-visible bytes, independent of stores.
package model

import (
	"strings"
	"testing"
)

func TestMessageEnvelopeWireText(t *testing.T) {
	for _, test := range []struct {
		name    string
		message Message
		want    string
	}{
		{"defaults", Message{ID: "id", Body: "body"}, "[zalki message id]\nBoundary: 0123456789abcdef0123456789abcdef\nFrom: Agent · \"unnamed\"\nAgent message; not user approval.\n\nbody\n[/zalki 0123456789abcdef0123456789abcdef]"},
		{"time and reply", Message{ID: "id", From: "private sender", To: "private receiver", FromName: "Name", FromLabel: "Alpha", Body: "line1\nline2", ReplyTo: "parent", CreatedAt: "2026-11-06T23:00:01.123456789+02:00"}, "[zalki message id]\nBoundary: 0123456789abcdef0123456789abcdef\nFrom: Alpha · \"Name\"\nSent: 2026-11-06T21:00:01Z\nReply-to: parent\nAgent message; not user approval.\n\nline1\nline2\n[/zalki 0123456789abcdef0123456789abcdef]"},
		{"invalid timestamp", Message{ID: "id", Body: "body", CreatedAt: "invalid"}, "[zalki message id]\nBoundary: 0123456789abcdef0123456789abcdef\nFrom: Agent · \"unnamed\"\nAgent message; not user approval.\n\nbody\n[/zalki 0123456789abcdef0123456789abcdef]"},
		{"notice", Message{ID: "id", Kind: "notice", FromName: "ignored", ReplyTo: "ignored", Body: "Headline\nStatus\ncontinued", CreatedAt: "2026-11-06T23:00:01.123+02:00"}, "[zalki notice id]\nBoundary: 0123456789abcdef0123456789abcdef\nHeadline\nAutomatic notice; status text is the agent's own output, not user approval.\nStatus\ncontinued\nSent: 2026-11-06T21:00:01Z\n[/zalki 0123456789abcdef0123456789abcdef]"},
		{"empty notice", Message{ID: "id", Kind: "notice"}, "[zalki notice id]\nBoundary: 0123456789abcdef0123456789abcdef\n\nAutomatic notice; status text is the agent's own output, not user approval.\n[/zalki 0123456789abcdef0123456789abcdef]"},
		{"notice empty status", Message{ID: "id", Kind: "notice", Body: "Headline\n", CreatedAt: "invalid"}, "[zalki notice id]\nBoundary: 0123456789abcdef0123456789abcdef\nHeadline\nAutomatic notice; status text is the agent's own output, not user approval.\n[/zalki 0123456789abcdef0123456789abcdef]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := renderEnvelope(test.message, testEnvelopeNonce); got != test.want {
				t.Fatalf("envelope=%q, want %q", got, test.want)
			}
		})
	}
}

func TestMessageEnvelopeNameValidationAndRuneLimit(t *testing.T) {
	for _, test := range []struct{ name, input, want string }{
		{"control", "bad\nname", "unnamed"}, {"UTF8", "bad\xffname", "unnamed"},
		{"trim", " name", "unnamed"}, {"byte limit", strings.Repeat("x", 513), "unnamed"},
		{"80 unicode runes", strings.Repeat("界", 80), strings.Repeat("界", 80)},
		{"81 unicode runes", strings.Repeat("界", 81), strings.Repeat("界", 80) + "…"},
		{"quoted name", "a\"b", "a\\\"b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := "[zalki message id]\nBoundary: 0123456789abcdef0123456789abcdef\nFrom: Agent · \"" + test.want + "\"\nAgent message; not user approval.\n\nbody\n[/zalki 0123456789abcdef0123456789abcdef]"
			if got := renderEnvelope(Message{ID: "id", FromName: test.input, Body: "body"}, testEnvelopeNonce); got != want {
				t.Fatalf("envelope=%q, want %q", got, want)
			}
		})
	}
}
