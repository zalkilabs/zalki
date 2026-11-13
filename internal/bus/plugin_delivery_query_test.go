package bus

import (
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func deliveryQueryFixture(t *testing.T, mode string) (Store, model.Session, string, model.Message) {
	t.Helper()
	s, to, key, m := pendingPluginFixture(t)
	m.Status = model.StatusContextEmitted
	m.Body = "Z6_BLOCKED_ACK_BODY_SENTINEL"
	s.PluginDelivery = denyPluginDelivery
	switch mode {
	case "missing-binding":
		m.PluginBinding = ""
	case "nil-checker":
		s.PluginDelivery = nil
	case "allowed":
		s.PluginDelivery = func(JournalView, string, model.Message) error { return nil }
	case "native":
		m.From, m.FromLabel, m.PluginBinding = "cursor:"+newID(), "Cursor", ""
		s.PluginDelivery = nil
	}
	// Model a durable pre-existing record, including a missing binding. Ordinary
	// rewrites intentionally refuse a binding change on an existing message.
	if err := s.locked(func() error { return s.writeRecord(filepath.Join(s.Dir, "messages", m.ID+".json"), m) }); err != nil {
		t.Fatal(err)
	}
	return s, to, key, m
}

func TestSessionPluginRawAckChecksBeforeResultAndMutation(t *testing.T) {
	for _, mode := range []string{"denied", "missing-binding", "nil-checker"} {
		t.Run(mode, func(t *testing.T) {
			s, to, key, m := deliveryQueryFixture(t, mode)
			got, err := s.Ack(to.ID, key, m.ID)
			if err == nil || got.Body != "" || got.ID != "" {
				t.Fatal("Z6_RAW_ACK_DENIED_NO_BODY_OR_RESULT", err)
			}
			stored, err := s.message(m.ID)
			if err != nil || stored.AcknowledgedAt != "" || stored.Status != m.Status {
				t.Fatal("Z6_RAW_ACK_DENIED_NO_STATE_CHANGE", err)
			}
		})
	}
}

func TestSessionPluginRawAckAllowedAndNative(t *testing.T) {
	for _, mode := range []string{"allowed", "native"} {
		s, to, key, m := deliveryQueryFixture(t, mode)
		got, err := s.Ack(to.ID, key, m.ID)
		if err != nil || got.Body != m.Body || got.AcknowledgedAt == "" {
			t.Fatal("Z6_RAW_ACK_ALLOWED_AND_NATIVE_UNCHANGED", mode, err)
		}
	}
}

func TestSessionPluginBlockedCursorCannotOrderVisibleRows(t *testing.T) {
	for _, mode := range []string{"denied", "missing-binding", "nil-checker"} {
		t.Run(mode, func(t *testing.T) {
			s, to, key, anchor := deliveryQueryFixture(t, mode)
			addNativeCursorNeighbors(t, s, anchor)
			query := MessageQuery{Actor: to.ID, Capability: key, History: true, IncludeBody: true, Cursor: anchor.ID}
			page, err := s.ReadMessages(query)
			if err == nil || len(page.Messages) != 0 {
				t.Fatal("Z6_BLOCKED_CURSOR_NO_ORDERING_OR_ROWS", err)
			}
			query.Cursor = ""
			page, err = s.ReadMessages(query)
			if err != nil || len(page.Messages) != 2 {
				t.Fatal("Z6_CURSOR_REFUSAL_DOES_NOT_HIDE_ALLOWED_ROWS", err)
			}
		})
	}
}

func TestSessionPluginAllowedAndNativeCursorRemainUsable(t *testing.T) {
	for _, mode := range []string{"allowed", "native"} {
		s, to, key, anchor := deliveryQueryFixture(t, mode)
		_, err := s.ReadMessages(MessageQuery{Actor: to.ID, Capability: key, History: true, Cursor: anchor.ID})
		if err != nil {
			t.Fatal("Z6_ALLOWED_AND_NATIVE_CURSOR_UNCHANGED", mode, err)
		}
	}
}

func addNativeCursorNeighbors(t *testing.T, s Store, anchor model.Message) {
	t.Helper()
	for _, stamp := range []string{"1970-01-01T00:00:00Z", "9999-01-01T00:00:00Z"} {
		visible := anchor
		visible.ID, visible.From, visible.FromLabel, visible.PluginBinding = newID(), "cursor:"+newID(), "Cursor", ""
		visible.CreatedAt = stamp
		if err := s.locked(func() error { return s.saveMessage(visible) }); err != nil {
			t.Fatal(err)
		}
	}
}
