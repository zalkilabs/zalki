package agent

import (
	"io"

	"github.com/zalkilabs/zalki/internal/model"
)

type HookInput struct {
	// Set only by trusted native normalization; JSON cannot select creation.
	AllowPromptRegistration bool     `json:"-"`
	SessionID               string   `json:"session_id"`
	CWD                     string   `json:"cwd"`
	Event                   string   `json:"hook_event_name"`
	Source                  string   `json:"source"`
	ConversationID          string   `json:"conversation_id"`
	WorkspaceRoots          []string `json:"workspace_roots"`
	// Status is a client-native field. Only the adapter's NormalizeHook and
	// HandleHook interpret it; core decides from the canonical Event.
	Status       string  `json:"status"`
	LoopCount    int     `json:"loop_count"`
	Prompt       string  `json:"prompt"`
	SessionTitle *string `json:"session_title"`
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
type StandardHooks struct{}

func (StandardHooks) NormalizeHook(event HookInput) (HookInput, error) { return event, nil }
func (StandardHooks) RegisterHook(HookInput) (HookRegistration, error) {
	return HookRegistration{}, nil
}
func (StandardHooks) HandleHook(HookHost, HookInput, string, io.Writer) (bool, error) {
	return false, nil
}
func (StandardHooks) StartupOutput(startup StartupContext) (any, error) {
	return map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "SessionStart", "additionalContext": startup.Instructions}}, nil
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
