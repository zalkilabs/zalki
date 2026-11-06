package model

import "errors"

// ReceiveCapabilities describes a client's declared receive behavior.
// All fields remain explicit in JSON, including false and empty values.
type ReceiveCapabilities struct {
	// IdleWake advertises the client's idle-wake capability.
	IdleWake bool `json:"idle_wake"`
	// Delivery names the declared delivery mechanism.
	Delivery string `json:"delivery"`
	// Inbox selects the native or journal-backed inbox.
	Inbox InboxMode `json:"inbox"`
	// BusyDelivery describes delivery timing while the client is busy.
	BusyDelivery DeliveryTiming `json:"busy_delivery"`
	// LifecycleHooks declares whether the client supplies lifecycle hooks.
	LifecycleHooks bool `json:"lifecycle_hooks"`
	// HookDelivery declares whether hooks emit the received message context.
	HookDelivery bool `json:"hook_delivery"`
	// RequiresListener declares that delivery needs a native listener lease.
	RequiresListener bool `json:"requires_listener"`
	// RequiresEndpoint declares that delivery needs a bound endpoint.
	RequiresEndpoint bool `json:"requires_endpoint"`
	// Experimental marks an experimental receive mechanism.
	Experimental bool `json:"experimental"`
}

// Validate checks the declared receive mode and delivery timing in order.
// It inspects capabilities; it does not change them or any session state.
func (c ReceiveCapabilities) Validate() error {
	if c.Delivery == "" || (c.Inbox != InboxNative && c.Inbox != InboxJournal) {
		return errors.New("delivery and inbox mode must be declared")
	}
	if c.HookDelivery && c.Inbox != InboxJournal {
		return errors.New("hook delivery requires a journal inbox")
	}
	switch c.BusyDelivery {
	case DeliveryNextTurn, DeliveryBetweenTools, DeliveryNextHook, DeliveryNotification:
	default:
		return errors.New("busy delivery timing must be declared")
	}
	return nil
}
