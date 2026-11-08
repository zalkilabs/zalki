package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestSentRecipientFilterPreviewAndPaging(t *testing.T) {
	store := fixtureStore(t)
	actor, key, peer, _ := registerPair(t, store, "cursor", "codex")
	other, _ := register(t, store, "cursor")
	if err := store.hookName(peer.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	body := "  Review\n\tשלום\r\n" + strings.Repeat("🙂", 30) + "\x00tail"
	var expected []string
	for i := range 9 {
		msg := Message{ID: newID(), From: actor.ID, To: peer.ID, Body: body, CreatedAt: fmt.Sprintf("2026-11-01T00:00:%02dZ", i), Status: model.StatusSubmitted}
		switch i % 3 {
		case 1:
			msg.To = other.ID
		case 2:
			msg.From, msg.To = peer.ID, actor.ID
		default:
			expected = append([]string{msg.ID}, expected...)
		}
		if err := store.saveMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	query := MessageQuery{Actor: actor.ID, Capability: key, Sent: true, To: "reviewer", Limit: 1}
	plain, err := store.ReadMessages(query)
	if err != nil || len(plain.Messages) != 1 || plain.Messages[0].BodyPreview != "" || plain.Messages[0].Body != "" {
		t.Fatal("preview was not opt-in", err)
	}
	query.Preview = true
	var seen []string
	for {
		page, err := store.ReadMessages(query)
		if err != nil || len(page.Messages) != 1 {
			t.Fatal("recipient filter did not run before the page limit", err)
		}
		msg := page.Messages[0]
		if msg.To != peer.ID || msg.From != actor.ID || msg.Body != "" || !msg.BodyOmitted || !strings.HasPrefix(msg.BodyPreview, "Review שלום ") || len(msg.BodyPreview) > 80 || !utf8.ValidString(msg.BodyPreview) || strings.ContainsFunc(msg.BodyPreview, unicode.IsControl) || strings.ContainsAny(msg.BodyPreview, "\r\n\t") {
			t.Fatalf("preview lost direction, omission, or UTF-8/single-line cap: %+v", msg)
		}
		seen = append(seen, msg.ID)
		stored, err := store.message(msg.ID)
		if err != nil || stored.Body != body || stored.BodyPreview != "" {
			t.Fatal("preview changed a canonical message", err)
		}
		encoded, _ := json.Marshal(msg)
		if strings.Contains(string(encoded), "BodyPreview") || strings.Contains(string(encoded), "preview") {
			t.Fatal("preview leaked into journal/CLI encoding")
		}
		if !page.More {
			break
		}
		query.Cursor = msg.ID
	}
	if strings.Join(seen, ",") != strings.Join(expected, ",") {
		t.Fatal("filter pagination lost records or newest-first order")
	}
	query.Cursor, query.MessageID, query.IncludeBody = "", expected[0], true
	exact, err := store.ReadMessages(query)
	if err != nil || len(exact.Messages) != 1 || exact.Messages[0].Body != body || exact.Messages[0].BodyPreview == "" {
		t.Fatal("exact opt-in preview lost the full body", err)
	}
	query.To = other.ID
	if _, err := store.ReadMessages(query); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("exact lookup ignored recipient filter", err)
	}
	query.MessageID, query.IncludeBody = "", false
	// Exact historical recipient IDs work without a surviving registration.
	path, err := store.sessionPath(peer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	query.To = peer.ID
	page, err := store.ReadMessages(query)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].To != peer.ID {
		t.Fatal("deleted recipient could not be filtered by canonical ID", err)
	}
	query.To = "reviewer"
	if _, err := store.ReadMessages(query); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatal("historical names unexpectedly resolved after deletion", err)
	}
}

func TestSentPreviewCannotExposeNativeInbox(t *testing.T) {
	store := fixtureStore(t)
	actor, key, peer, peerKey := registerPair(t, store, "codex", "cursor")
	msg := Message{ID: newID(), From: peer.ID, To: actor.ID, TargetGeneration: actor.Generation, Body: "native secret", CreatedAt: model.Timestamp(), Status: model.StatusSubmitted}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	for _, query := range []MessageQuery{
		{Actor: actor.ID, Capability: key, Preview: true},
		{Actor: actor.ID, Capability: key, History: true, Preview: true, MessageID: msg.ID},
		{Actor: actor.ID, Capability: key, To: peer.ID},
	} {
		if _, err := store.ReadMessages(query); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("sent-only options accepted for an inbox", err)
		}
	}
	page, err := store.ReadMessages(MessageQuery{Actor: actor.ID, Capability: key, Sent: true, Preview: true})
	if err != nil || len(page.Messages) != 0 {
		t.Fatal("sent preview exposed incoming native body", err)
	}
	if _, err := store.ReadMessages(MessageQuery{Actor: actor.ID, Capability: key, Sent: true, Preview: true, MessageID: msg.ID}); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("sent preview exposed exact incoming native body", err)
	}
	page, err = store.ReadMessages(MessageQuery{Actor: peer.ID, Capability: peerKey, Sent: true, Preview: true, MessageID: msg.ID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].BodyPreview != "native secret" {
		t.Fatal("sender could not preview its own sent body", err)
	}
	msg.ID, msg.From = newID(), actor.ID
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	page, err = store.ReadMessages(MessageQuery{Actor: actor.ID, Capability: key, Sent: true, Preview: true, MessageID: msg.ID})
	if err != nil || len(page.Messages) != 1 || !page.Messages[0].BodyDeferred || page.Messages[0].BodyPreview != "" {
		t.Fatal("self-sent native body escaped deferred delivery", err)
	}
}
