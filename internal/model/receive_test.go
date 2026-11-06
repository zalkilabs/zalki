// These in-package tables pin capability validation order and ordinary error
// text, including combinations the current validator deliberately accepts.
package model

import (
	"errors"
	"testing"
)

func TestReceiveCapabilitiesValidation(t *testing.T) {
	for _, test := range []struct {
		name  string
		value ReceiveCapabilities
		want  string
	}{
		{"native next turn", ReceiveCapabilities{Delivery: "custom", Inbox: "native", BusyDelivery: "next_turn"}, ""},
		{"native between tools", ReceiveCapabilities{Delivery: "custom", Inbox: "native", BusyDelivery: "between_tools"}, ""},
		{"journal next hook", ReceiveCapabilities{Delivery: "custom", Inbox: "journal", BusyDelivery: "next_hook", HookDelivery: true}, ""},
		{"native notification", ReceiveCapabilities{Delivery: "custom", Inbox: "native", BusyDelivery: "native_notification"}, ""},
		{"other flags are not constrained", ReceiveCapabilities{Delivery: "custom", Inbox: "journal", BusyDelivery: "next_hook", IdleWake: true, LifecycleHooks: true, HookDelivery: true, RequiresListener: true, RequiresEndpoint: true, Experimental: true}, ""},
		{"nonempty delivery is not trimmed", ReceiveCapabilities{Delivery: " ", Inbox: "native", BusyDelivery: "next_turn"}, ""},
		{"zero", ReceiveCapabilities{}, "delivery and inbox mode must be declared"},
		{"empty delivery", ReceiveCapabilities{Inbox: "native", BusyDelivery: "next_turn"}, "delivery and inbox mode must be declared"},
		{"empty inbox", ReceiveCapabilities{Delivery: "custom", BusyDelivery: "next_turn"}, "delivery and inbox mode must be declared"},
		{"unknown inbox", ReceiveCapabilities{Delivery: "custom", Inbox: "unknown", BusyDelivery: "next_turn"}, "delivery and inbox mode must be declared"},
		{"native hook delivery", ReceiveCapabilities{Delivery: "custom", Inbox: "native", BusyDelivery: "next_turn", HookDelivery: true}, "hook delivery requires a journal inbox"},
		{"empty timing", ReceiveCapabilities{Delivery: "custom", Inbox: "journal"}, "busy delivery timing must be declared"},
		{"unknown timing", ReceiveCapabilities{Delivery: "custom", Inbox: "journal", BusyDelivery: "unknown"}, "busy delivery timing must be declared"},
		{"delivery before hook and timing", ReceiveCapabilities{Inbox: "native", HookDelivery: true}, "delivery and inbox mode must be declared"},
		{"inbox before hook and timing", ReceiveCapabilities{Delivery: "custom", Inbox: "unknown", HookDelivery: true}, "delivery and inbox mode must be declared"},
		{"hook before timing", ReceiveCapabilities{Delivery: "custom", Inbox: "native", HookDelivery: true}, "hook delivery requires a journal inbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := test.value
			err := value.Validate()
			if value != test.value {
				t.Error("Validate changed its input")
			}
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != test.want {
				t.Fatalf("Validate()=%v, want %q", err, test.want)
			}
			if errors.Unwrap(err) != nil {
				t.Fatalf("Validate added an error wrapper: %v", err)
			}
		})
	}
}
