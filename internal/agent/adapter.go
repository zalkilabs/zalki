package agent

import (
	"context"

	"github.com/zalkilabs/zalki/internal/model"
)

// Lifecycle states how an adapter's client behaves, so core never switches on
// an event name for one client. The zero value is the default: a session end
// retires the session, and a prompt hook cannot register an unknown session.
type Lifecycle struct {
	// KeepOnSessionEnd keeps the session listed and ready on SessionEnd. Use it
	// when the client also ends sessions that are only unloaded, not closed.
	KeepOnSessionEnd bool
	// PromptMayRegister lets a trusted prompt hook register a session core has
	// not seen yet. Only core sets this on an event; JSON input cannot.
	PromptMayRegister bool
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
	// core has no default. The registry refuses a key for an adapter with no hooks.
	HookEventKey string
	Hooks        map[string]string // Native event name to the common lifecycle event.
	Lifecycle    Lifecycle
	Native       []NativeField
	ProcessNames []string
	OwnerRole    string
	Executable   string
	Notes        []string
}

type Adapter interface {
	Descriptor() AdapterDescriptor
	ReceiveCapabilities(model.Session) model.ReceiveCapabilities
	Deliver(context.Context, model.Message, model.Session) model.Receipt
}
