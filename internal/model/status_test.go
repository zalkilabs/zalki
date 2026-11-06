// These in-package tests pin model's wire spellings and value predicates with
// independent literals. The bus characterizations separately pin its aliases.
package model

import "testing"

func TestStateValuePredicates(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"ready", true}, {"busy", true}, {"approval", true}, {"retired", true},
		{"", false}, {"invented", false}, {"READY", false},
	} {
		t.Run(test.value, func(t *testing.T) {
			if got := SessionState(test.value).Valid(); got != test.valid {
				t.Fatalf("SessionState(%q).Valid()=%v, want %v", test.value, got, test.valid)
			}
		})
	}
}

func TestDeliveryValuePredicates(t *testing.T) {
	for _, test := range []struct {
		value       string
		valid, ack  bool
		hookPending bool
	}{
		{"attempting", true, true, false},
		{"submitted", true, true, false},
		{"transport_written", true, true, false},
		{"pending_next_activity", true, true, true},
		{"wake_hint_written", true, true, true},
		{"wake_hint_uncertain", true, true, true},
		{"hook_context_emitted", true, true, false},
		{"not_sent", true, false, false},
		{"not_sent_pending_approval", true, false, false},
		{"queued_for_approval", false, false, false},
		{"command_failed", true, false, false},
		{"unknown_after_timeout", true, true, false},
		{"unknown_after_write", true, true, false},
		{"", false, false, false},
		{"invented", false, false, false},
		{"SUBMITTED", false, false, false},
	} {
		t.Run(test.value, func(t *testing.T) {
			value := DeliveryStatus(test.value)
			if got := value.Valid(); got != test.valid {
				t.Errorf("Valid()=%v, want %v", got, test.valid)
			}
			if got := value.CanAcknowledge(); got != test.ack {
				t.Errorf("CanAcknowledge()=%v, want %v", got, test.ack)
			}
			if got := HookPendingStatus(value); got != test.hookPending {
				t.Errorf("HookPendingStatus()=%v, want %v", got, test.hookPending)
			}
		})
	}
}

func TestEnumWireSpellings(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
	}{
		{"StateReady", string(StateReady), "ready"},
		{"StateBusy", string(StateBusy), "busy"},
		{"StateApproval", string(StateApproval), "approval"},
		{"StateRetired", string(StateRetired), "retired"},
		{"StatusAttempting", string(StatusAttempting), "attempting"},
		{"StatusSubmitted", string(StatusSubmitted), "submitted"},
		{"StatusTransportWritten", string(StatusTransportWritten), "transport_written"},
		{"StatusPendingActivity", string(StatusPendingActivity), "pending_next_activity"},
		{"StatusWakeWritten", string(StatusWakeWritten), "wake_hint_written"},
		{"StatusWakeUncertain", string(StatusWakeUncertain), "wake_hint_uncertain"},
		{"StatusContextEmitted", string(StatusContextEmitted), "hook_context_emitted"},
		{"StatusNotSent", string(StatusNotSent), "not_sent"},
		{"StatusApprovalBlocked", string(StatusApprovalBlocked), "not_sent_pending_approval"},
		{"StatusQueuedForApproval", string(StatusQueuedForApproval), "queued_for_approval"},
		{"StatusCommandFailed", string(StatusCommandFailed), "command_failed"},
		{"StatusUnknownTimeout", string(StatusUnknownTimeout), "unknown_after_timeout"},
		{"StatusUnknownWrite", string(StatusUnknownWrite), "unknown_after_write"},
		{"InboxNative", string(InboxNative), "native"},
		{"InboxJournal", string(InboxJournal), "journal"},
		{"DeliveryNextTurn", string(DeliveryNextTurn), "next_turn"},
		{"DeliveryBetweenTools", string(DeliveryBetweenTools), "between_tools"},
		{"DeliveryNextHook", string(DeliveryNextHook), "next_hook"},
		{"DeliveryNotification", string(DeliveryNotification), "native_notification"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.value != test.want {
				t.Fatalf("%s=%q, want %q", test.name, test.value, test.want)
			}
		})
	}
}
