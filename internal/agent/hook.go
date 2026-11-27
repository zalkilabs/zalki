package agent

import (
	"encoding/json"
	"io"

	"github.com/zalkilabs/zalki/internal/model"
)

type HookInput struct {
	// Set only by core from the adapter's Lifecycle; JSON cannot select creation.
	AllowPromptRegistration bool   `json:"-"`
	SessionID               string `json:"session_id"`
	CWD                     string `json:"cwd"`
	Event                   string `json:"hook_event_name"`
	Source                  string `json:"source"`
	// Native is a JSON object with the client-specific hook fields the adapter
	// declared in its descriptor (AdapterDescriptor.Native), and only those.
	// Core checks their types and keeps nothing else. Only the adapter's
	// NormalizeHook and HandleHook interpret it; core decides from the
	// canonical Event.
	Native       json.RawMessage `json:"-"`
	Prompt       string          `json:"prompt"`
	SessionTitle *string         `json:"session_title"`
	// LastAssistantMessage is optional turn text from a Stop hook. Only a short
	// single line is forwarded, and only when a completion watch is waiting.
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
}

type StartupContext struct {
	Actor, Capability, StateDir, ConfigFile, Instructions string
	// Executable is captured before core takes the journal lock for pure rendering.
	Executable string
}

// HookRegistration is what an adapter reports when a lifecycle hook registers a session.
type HookRegistration struct {
	Endpoint           *model.Endpoint
	PreserveGeneration bool
}

// HookAdapter normalizes lifecycle hooks and supplies client startup output.
type HookAdapter interface {
	NormalizeHook(HookInput) (HookInput, error)
	RegisterHook(HookInput) (HookRegistration, error)
	HandleHook(HookHost, HookInput, string, io.Writer) (bool, error)
	StartupOutput(StartupContext) (any, error)
}

// HookPayloadAdapter optionally opts one native event into the bounded streaming
// selector. Core validates a complete object, discards only the top-level
// tool_input/tool_response values, and caps wire input at 32 MiB and retained
// metadata at 8 MiB. Other events keep 8 MiB admission; an empty event opts out.
// The policy comes from the trusted adapter, never the incoming JSON.
type HookPayloadAdapter interface {
	ExtendedHookEvent() string
}

// HookHost exposes bounded lifecycle operations, never the service, registry,
// transport override, or arbitrary journal read/write access. Core's host
// requires a nonempty bound actor and an identical supplied id for each
// operation, rejecting mismatches before Store calls or prompt parsing.
// Adapters that own client state may additionally use LifecycleHost, not
// arbitrary credential reads or rotation.
type HookHost interface {
	UpdateSession(string, model.SessionState, bool) error
	EmitPending(string) (string, error)
	CompletedNativePrompt(string, string) (string, error)
}

// StandardHooks supplies default lifecycle behavior for adapters that need none of their own.
// It has no StartupOutput: what a client reads at session start is that client's own format,
// so every hook adapter writes its own.
type StandardHooks struct{}

func (StandardHooks) NormalizeHook(event HookInput) (HookInput, error) { return event, nil }
func (StandardHooks) RegisterHook(HookInput) (HookRegistration, error) {
	return HookRegistration{}, nil
}
func (StandardHooks) HandleHook(HookHost, HookInput, string, io.Writer) (bool, error) {
	return false, nil
}

// StandardEvents lists the common lifecycle events adapters may map native hook names onto.
func StandardEvents(interrupt bool) map[string]string {
	events := map[string]string{}
	for _, event := range []string{"SessionStart", "SessionEnd", "PermissionRequest", "PostToolUse", "UserPromptSubmit", "Stop"} {
		events[event] = event
	}
	if interrupt {
		events["Interrupt"] = "Interrupt"
	}
	return events
}
