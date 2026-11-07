// These checks exercise in-memory mutations without stores, adapters or clocks hooks.
package model

import (
	"errors"
	"testing"
)

func TestMessageHoldAndNativeRelease(t *testing.T) {
	msg := Message{ID: "id", Body: "body", Status: "attempting", AcknowledgedAt: "ack"}
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	if !msg.IsQueuedForApproval() || msg.Status != "not_sent_pending_approval" || msg.Receipt.Status != "not_sent_pending_approval" || msg.Receipt.Error != "queued until the permission prompt is answered" {
		t.Fatalf("hold=%+v", msg)
	}
	requireReceiptClock(t, msg.Receipt.At)
	if err := msg.ReleaseHeld("attempting"); err != nil || msg.Held || msg.Status != "attempting" || msg.Receipt != nil {
		t.Fatalf("native release=%+v, error=%v", msg, err)
	}
	if msg.ID != "id" || msg.Body != "body" || msg.AcknowledgedAt != "ack" {
		t.Fatal("native release changed unrelated message fields")
	}
}

func TestMessageHookReleaseAndContextEmission(t *testing.T) {
	msg := Message{ID: "id", Body: "body", Status: "attempting", AcknowledgedAt: "ack"}
	if err := msg.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := msg.ReleaseHeld("pending_next_activity"); err != nil || msg.Held || msg.Status != "pending_next_activity" || msg.Receipt.Status != "pending_next_activity" || msg.Receipt.Error != "" {
		t.Fatalf("hook release=%+v, error=%v", msg, err)
	}
	requireReceiptClock(t, msg.Receipt.At)
	if err := msg.EmitContext(); err != nil || msg.Status != "hook_context_emitted" || msg.Receipt.Status != "hook_context_emitted" {
		t.Fatalf("emission=%+v, error=%v", msg, err)
	}
	requireReceiptClock(t, msg.Receipt.At)
	if msg.ID != "id" || msg.Body != "body" || msg.AcknowledgedAt != "ack" {
		t.Fatal("hook release or emission changed unrelated message fields")
	}
}

func TestMessageLateReceiptPreservesEmittedContext(t *testing.T) {
	msg := Message{ID: "id", Body: "body", Status: "pending_next_activity", AcknowledgedAt: "ack"}
	if err := msg.EmitContext(); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("live cause")
	receipt := Receipt{At: "late", Status: "wake_hint_written", Error: "public error", Output: "native output"}.WithCause(cause)
	if err := msg.ApplyReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	if msg.Status != "hook_context_emitted" || msg.Receipt.Status != "wake_hint_written" || msg.Receipt.At != "late" || msg.Receipt.Code != "unavailable" || msg.Receipt.Output != "native output" {
		t.Fatalf("late receipt=%+v, message=%+v", msg.Receipt, msg)
	}
	if msg.Receipt.Cause() != cause { //nolint:errorlint // The copied receipt must retain exact diagnostic identity.
		t.Fatal("ApplyReceipt lost cause identity")
	}
	if receipt.Code != "" || msg.Held || msg.ID != "id" || msg.Body != "body" || msg.AcknowledgedAt != "ack" {
		t.Fatal("ApplyReceipt changed its input or unrelated message fields")
	}
}

func TestMessageSettlementUsesSuppliedDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name   string
		status DeliveryStatus
		held   bool
		code   ErrorCode
		reason string
	}{
		{"empty diagnostics", "attempting", false, "", ""},
		{"held refusal", "not_sent_pending_approval", true, "not_found", "reason"},
		{"future code", "attempting", true, "future_code", "text"},
	} {
		t.Run(test.name, func(t *testing.T) {
			msg := Message{Status: test.status, Held: test.held, Receipt: &Receipt{Output: "old"}, AcknowledgedAt: "ack"}
			if err := msg.SettleNotSent(test.code, test.reason); err != nil {
				t.Fatal(err)
			}
			if msg.Status != "not_sent" || msg.Held || msg.Receipt.Status != "not_sent" || msg.Receipt.Code != test.code || msg.Receipt.Error != test.reason || msg.Receipt.Output != "" || msg.AcknowledgedAt != "ack" {
				t.Fatalf("settled=%+v receipt=%+v", msg, msg.Receipt)
			}
			requireReceiptClock(t, msg.Receipt.At)
		})
	}
}

func TestMessageReceiptDiagnosticFallback(t *testing.T) {
	for _, test := range []struct {
		name, errorText string
		code, want      ErrorCode
	}{
		{"missing code", "public error", "", "unavailable"},
		{"explicit future code", "public error", "future_code", "future_code"},
		{"no error", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			msg := Message{Status: "attempting", Held: true}
			receipt := Receipt{At: "", Status: "submitted", Code: test.code, Error: test.errorText}
			if err := msg.ApplyReceipt(receipt); err != nil {
				t.Fatal(err)
			}
			if msg.Status != "submitted" || !msg.Held || msg.Receipt.Code != test.want || msg.Receipt.At != "" || receipt.Code != test.code || msg.Receipt == &receipt {
				t.Fatalf("receipt copy or fallback changed: message=%+v receipt=%+v", msg, msg.Receipt)
			}
		})
	}
}

func TestMessageQueuedViewRequiresHeldBlockedState(t *testing.T) {
	for _, test := range []struct {
		status     DeliveryStatus
		held, want bool
	}{
		{"not_sent_pending_approval", true, true}, {"not_sent_pending_approval", false, false},
		{"queued_for_approval", true, false}, {"submitted", true, false},
	} {
		if got := (Message{Status: test.status, Held: test.held}).IsQueuedForApproval(); got != test.want {
			t.Fatalf("status=%q held=%v queued=%v, want %v", test.status, test.held, got, test.want)
		}
	}
}
