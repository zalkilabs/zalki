// These literal cases pin transition precedence and refusal atomicity.
package model

import (
	"errors"
	"reflect"
	"testing"
)

func TestMessageTransitionOrder(t *testing.T) {
	for _, test := range []struct {
		name      string
		from, to  DeliveryStatus
		held      bool
		want      DeliveryStatus
		errorText string
	}{
		{"held attempt exception", "not_sent_pending_approval", "attempting", true, "attempting", ""},
		{"held hook exception", "not_sent_pending_approval", "pending_next_activity", true, "pending_next_activity", ""},
		{"held refusal exception", "not_sent_pending_approval", "not_sent", true, "not_sent", ""},
		{"legacy attempt", "not_sent_pending_approval", "attempting", false, "not_sent_pending_approval", `invalid_transition: invalid delivery receipt "attempting"`},
		{"two invalid states", "future-from", "future-to", false, "future-from", `invalid_transition: invalid delivery receipt "future-to"`},
		{"same attempting invalid", "attempting", "attempting", false, "attempting", `invalid_transition: invalid delivery receipt "attempting"`},
		{"response before emitted guard", "hook_context_emitted", "queued_for_approval", false, "hook_context_emitted", `invalid_transition: invalid delivery receipt "queued_for_approval"`},
		{"late pending", "hook_context_emitted", "pending_next_activity", false, "hook_context_emitted", ""},
		{"late wake", "hook_context_emitted", "wake_hint_written", false, "hook_context_emitted", ""},
		{"late uncertain wake", "hook_context_emitted", "wake_hint_uncertain", false, "hook_context_emitted", ""},
		{"same emitted", "hook_context_emitted", "hook_context_emitted", false, "hook_context_emitted", ""},
		{"same submitted", "submitted", "submitted", false, "submitted", ""},
		{"attempt refusal", "attempting", "not_sent", false, "not_sent", ""},
		{"pending to wake", "pending_next_activity", "wake_hint_written", false, "wake_hint_written", ""},
		{"uncertain to emitted", "wake_hint_uncertain", "hook_context_emitted", false, "hook_context_emitted", ""},
		{"legacy cannot reopen", "not_sent_pending_approval", "pending_next_activity", false, "not_sent_pending_approval", `invalid_transition: cannot change delivery status from "not_sent_pending_approval" to "pending_next_activity"`},
		{"submitted cannot emit", "submitted", "hook_context_emitted", false, "submitted", `invalid_transition: cannot change delivery status from "submitted" to "hook_context_emitted"`},
		{"held flag alone insufficient", "submitted", "pending_next_activity", true, "submitted", `invalid_transition: cannot change delivery status from "submitted" to "pending_next_activity"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			next, err := Transition(test.from, test.to, test.held)
			if next != test.want {
				t.Fatalf("next=%q, want %q", next, test.want)
			}
			if test.errorText == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != "invalid_transition" || err.Error() != test.errorText {
				t.Fatalf("error=%v, want typed invalid_transition with text %q", err, test.errorText)
			}
		})
	}
}

func requireMessageRefusalUnchanged(t *testing.T, got, before Message, priorReceipt Receipt) {
	t.Helper()
	if !reflect.DeepEqual(got, before) || got.Receipt != before.Receipt || !reflect.DeepEqual(*got.Receipt, priorReceipt) {
		t.Fatalf("refused operation mutated message or its prior receipt: got=%+v, before=%+v", got, before)
	}
}

func TestMessageReleaseHeldGuardPrecedesTransition(t *testing.T) {
	for _, test := range []struct {
		name   string
		status DeliveryStatus
		held   bool
		to     DeliveryStatus
	}{
		{"legacy plus invalid target", "not_sent_pending_approval", false, "future"},
		{"wrong source plus invalid target", "submitted", true, "future"},
		{"held plus invalid target", "not_sent_pending_approval", true, "future"},
		{"legacy plus valid target", "not_sent_pending_approval", false, "attempting"},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := &Receipt{At: "old", Status: "submitted", Output: "keep"}
			msg := Message{ID: "id", Status: test.status, Held: test.held, Receipt: receipt, AcknowledgedAt: "ack"}
			before, prior := msg, *receipt
			err := msg.ReleaseHeld(test.to)
			if err == nil || err.Error() != "invalid_transition: only a held message may return to attempting or pending delivery" {
				t.Fatalf("guard error=%v", err)
			}
			requireMessageRefusalUnchanged(t, msg, before, prior)
		})
	}
}

func TestMessageRefusedOperationsPreserveReceipt(t *testing.T) {
	for _, test := range []struct {
		name  string
		apply func(*Message) error
		want  string
	}{
		{"hold", func(m *Message) error { return m.Hold() }, `invalid_transition: cannot change delivery status from "submitted" to "not_sent_pending_approval"`},
		{"settle", func(m *Message) error { return m.SettleNotSent("unavailable", "new") }, `invalid_transition: cannot change delivery status from "submitted" to "not_sent"`},
		{"emit", func(m *Message) error { return m.EmitContext() }, `invalid_transition: cannot change delivery status from "submitted" to "hook_context_emitted"`},
		{"invalid receipt and missing diagnostic code", func(m *Message) error { return m.ApplyReceipt(Receipt{Status: "future", Error: "new", Output: "new"}) }, `invalid_transition: invalid delivery receipt "future"`},
		{"refused transition and missing diagnostic code", func(m *Message) error { return m.ApplyReceipt(Receipt{Status: "pending_next_activity", Error: "new"}) }, `invalid_transition: cannot change delivery status from "submitted" to "pending_next_activity"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := &Receipt{At: "old", Status: "submitted", Code: "not_found", Error: "old", Output: "keep"}
			msg := Message{ID: "id", Body: "body", Status: "submitted", Held: true, Receipt: receipt, AcknowledgedAt: "ack"}
			before, prior := msg, *receipt
			if err := test.apply(&msg); err == nil || err.Error() != test.want {
				t.Fatalf("error=%v, want %s", err, test.want)
			}
			requireMessageRefusalUnchanged(t, msg, before, prior)
		})
	}
}
