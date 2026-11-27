package agent

import (
	"context"

	"github.com/zalkilabs/zalki/internal/model"
)

// Lifecycle states how an adapter's client behaves, so core never switches on
// an event name for one client. The zero value is the default: a session end
// retires the session, and neither a prompt hook nor an Activity hook can
// register an unknown session.
type Lifecycle struct {
	// KeepOnSessionEnd keeps the session listed and ready on SessionEnd. Use it
	// when the client also ends sessions that are only unloaded, not closed.
	KeepOnSessionEnd bool
	// PromptMayRegister lets a trusted prompt hook register a session core has
	// not seen yet. Only core sets this on an event; JSON input cannot.
	PromptMayRegister bool
	// ActivityMayRegister lets a trusted Activity hook register a session core has
	// not seen yet, and allows the adapter's policy to reactivate a retired one on
	// it. Activity is the common event for a client that has no prompt hook but
	// shows that it works for a chat (a model call starts). Like a prompt hook, an
	// Activity hook that may register never fails the client: errors are logged.
	// Only core sets the flag on an event; JSON input cannot.
	ActivityMayRegister bool
	// PromptReply is the one JSON object, on one line without the newline, that
	// the client needs on stdout after a prompt hook that may register a session,
	// whatever the outcome of the hook. Core writes it and a newline, but only
	// when the adapter's own handler wrote nothing. Empty means core writes
	// nothing. It needs PromptMayRegister. Core holds no client reply of its own.
	PromptReply string
}

// NativeKind is the JSON type core expects for a native hook field.
type NativeKind int

const (
	NativeString NativeKind = iota
	NativeStrings
	NativeInt
)

// NativeCapture says how contract capture records a native field without its
// private value.
type NativeCapture int

const (
	CaptureNone  NativeCapture = iota // the field is not recorded
	CaptureID                         // a placeholder identity
	CapturePaths                      // one placeholder path
)

// NativeField names a top-level hook key an adapter reads from HookInput.Native.
// Core keeps only declared keys and rejects a value of the wrong type, as it
// does for the common fields. Native fields are not supported together with
// HookPayloadAdapter.
type NativeField struct {
	Key     string
	Kind    NativeKind
	Capture NativeCapture
}

type AdapterDescriptor struct {
	Kind, Label string
	// HookEventKey is the top-level key of the hook JSON that holds the native
	// event name: "hook_event_name" for Claude, Codex and Cursor, "hookEventName"
	// for a client that sends camelCase. Every adapter with Hooks declares it;
	// core has no default. The registry refuses an adapter with hooks and no key,
	// and a key for an adapter with no hooks.
	HookEventKey string
	Hooks        map[string]string // Native event name to the common lifecycle event.
	Lifecycle    Lifecycle
	Native       []NativeField
	ProcessNames []string
	OwnerRole    string
	Executable   string
	Notes        []string

	// UncertainReceiver and UncertainAcknowledged are the sentences core shows
	// for a message to this client whose delivery is unconfirmed (it may have
	// arrived): to the receiver, and after the receiver ACKed it. Each has one %s
	// for the message id. Empty means core adds no client-specific sentence.
	UncertainReceiver, UncertainAcknowledged string

	// InstallNotice is one line the install command prints after it has applied
	// the configuration, for a client that needs the user to do something next
	// (for example to trust the hooks). Empty means no line. One line, no control
	// characters.
	InstallNotice string
}

type Adapter interface {
	Descriptor() AdapterDescriptor
	ReceiveCapabilities(model.Session) model.ReceiveCapabilities
	Deliver(context.Context, model.Message, model.Session) model.Receipt
}
