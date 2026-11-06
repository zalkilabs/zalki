package model

// InboxMode identifies the native or journal inbox used by a client.
type InboxMode string

// DeliveryTiming describes when a busy client can receive a message.
type DeliveryTiming string

const (
	// InboxNative identifies a client-provided inbox.
	InboxNative InboxMode = "native"
	// InboxJournal identifies a journal-backed inbox.
	InboxJournal InboxMode = "journal"
	// DeliveryNextTurn identifies delivery on the next client turn.
	DeliveryNextTurn DeliveryTiming = "next_turn"
	// DeliveryBetweenTools identifies delivery between client tool calls.
	DeliveryBetweenTools DeliveryTiming = "between_tools"
	// DeliveryNextHook identifies delivery on the next lifecycle hook.
	DeliveryNextHook DeliveryTiming = "next_hook"
	// DeliveryNotification identifies a native delivery notification.
	DeliveryNotification DeliveryTiming = "native_notification"
)
