package pluginfeed

import (
	"context"
	"encoding/json"
	"io"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/clientconfig"
	"github.com/zalkilabs/zalki/internal/model"
)

// stubStartup gives test adapters the startup output of the core tests. It is test
// code: core has no default startup output, every adapter renders its own.
type stubStartup struct{ agent.StandardHooks }

func (stubStartup) StartupOutput(startup agent.StartupContext) (any, error) {
	return map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "SessionStart", "additionalContext": startup.Instructions}}, nil
}

// stubCodexAdapter stands in for the real Codex adapter, which lives in internal/adapters
// and cannot be imported here without an import cycle. It supplies dispatch
// metadata only; lifecycle policy is tested with Builtin in internal/adapters.
type stubCodexAdapter struct{ stubStartup }

func (stubCodexAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "codex", Label: "Codex", HookEventKey: "hook_event_name", Hooks: agent.StandardEvents(true), ProcessNames: []string{"codex"}, OwnerRole: "backend", Executable: "codex"}
}

func (stubCodexAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{IdleWake: true, Delivery: "native_queue_next_turn", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn, LifecycleHooks: true}
}

func (stubCodexAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}

func (stubCodexAdapter) Health(agent.HealthHost, model.Session) (string, string) {
	return "native_queue_only", ""
}

// stubClaudeAdapter provides the native-inbox dispatch contract for core tests.
// Real socket, metadata, configuration and hook registration tests live in adapters.
type stubClaudeAdapter struct{ stubStartup }

func (stubClaudeAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "claude", Label: "Claude", HookEventKey: "hook_event_name", Hooks: agent.StandardEvents(false), ProcessNames: []string{"claude"}, OwnerRole: "client", Executable: "claude"}
}

func (stubClaudeAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{IdleWake: true, Delivery: "experimental_socket", Inbox: model.InboxNative, BusyDelivery: model.DeliveryBetweenTools, LifecycleHooks: true, Experimental: true, RequiresEndpoint: true}
}

func (stubClaudeAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	panic("core Claude stub requires an explicit test transport")
}

// stubCursorAdapter supplies only the core dispatch/lifecycle contract. Socket
// sender, metadata, configuration and output-policy tests use the real Builtin.
type stubCursorAdapter struct{ agent.StandardHooks }

func (stubCursorAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "cursor", Label: "Cursor", HookEventKey: "hook_event_name", ProcessNames: []string{"agent", "cursor-agent"}, OwnerRole: "client", Executable: "agent", Lifecycle: agent.Lifecycle{PromptMayRegister: true, PromptReply: `{"continue":true}`}, Native: stubNativeFields, Hooks: map[string]string{"sessionStart": "SessionStart", "sessionEnd": "SessionEnd", "beforeSubmitPrompt": "UserPromptSubmit", "postToolUse": "PostToolUse", "stop": "Stop"}}
}
func (stubCursorAdapter) ReceiveCapabilities(v model.Session) model.ReceiveCapabilities {
	c := model.ReceiveCapabilities{Delivery: "next_postToolUse_or_completed_stop_hook", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, LifecycleHooks: true, HookDelivery: true, RequiresListener: true}
	if v.Listener != nil {
		c.IdleWake, c.Delivery, c.BusyDelivery = true, "native_background_shell_output", model.DeliveryNotification
	}
	return c
}
func (stubCursorAdapter) Deliver(_ context.Context, _ model.Message, v model.Session) model.Receipt {
	if v.Listener != nil {
		panic("core Cursor stub cannot simulate native wake acceptance")
	}
	return model.NewReceipt(model.StatusPendingActivity)
}
func (stubCursorAdapter) NormalizeHook(v agent.HookInput) (agent.HookInput, error) {
	n := stubNativeOf(v)
	if v.Event == "Stop" && n.Status != "" && n.Status != "completed" {
		v.Event = "Interrupt"
	}
	if v.SessionID == "" {
		v.SessionID = n.ConversationID
	}
	if v.CWD == "" && len(n.WorkspaceRoots) > 0 {
		v.CWD = n.WorkspaceRoots[0]
	}
	return v, nil
}
func (stubCursorAdapter) HandleHook(host agent.HookHost, v agent.HookInput, id string, _ io.Writer) (bool, error) {
	h := host.(agent.LifecycleHost)
	n := stubNativeOf(v)
	if v.Event == "UserPromptSubmit" {
		return true, h.ApplyLifecycle(stubResumeOnPrompt)
	}
	if v.Event == "Interrupt" || v.Event == "Stop" && (n.Status != "completed" || n.LoopCount >= 5) {
		return true, h.ApplyLifecycle(stubSuppressRecovery)
	}
	return false, nil
}

// The stub Cursor policies mirror the real adapter's so core transaction tests
// exercise realistic state; the adapter's own policy is tested in adapters.
func stubResumeOnPrompt(v model.SessionView) (agent.LifecycleChange, error) {
	state := cursorState(model.Session{AdapterState: v.AdapterState})
	if state.RecoverySuppressed {
		state.RecoveryAttempted = false
	}
	state.RecoverySuppressed = false
	next := model.Session{}
	setCursorState(&next, state)
	return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate, AdapterState: next.AdapterState}, nil
}

func stubSuppressRecovery(v model.SessionView) (agent.LifecycleChange, error) {
	state := cursorState(model.Session{AdapterState: v.AdapterState})
	state.RecoverySuppressed = true
	next := model.Session{}
	setCursorState(&next, state)
	return agent.LifecycleChange{Transition: agent.TransitionReady, AdapterState: next.AdapterState}, nil
}

func stubClaimRecovery(v model.SessionView) (agent.RecoveryDecision, error) {
	state := cursorState(model.Session{AdapterState: v.AdapterState})
	if state.RecoverySuppressed || state.RecoveryAttempted {
		return agent.RecoveryDecision{}, nil
	}
	state.RecoveryAttempted = true
	next := model.Session{}
	setCursorState(&next, state)
	return agent.RecoveryDecision{Recover: true, AdapterState: next.AdapterState}, nil
}

func (stubCursorAdapter) StartupOutput(v agent.StartupContext) (any, error) {
	return map[string]any{"additional_context": v.Instructions, "env": map[string]string{"ZALKI_ACTOR": v.Actor, "ZALKI_CAPABILITY": v.Capability}}, nil
}

// CursorListenerState mirrors the Cursor adapter's private state so core tests
// can drive realistic adapter state; core itself never interprets it.
type CursorListenerState struct {
	Schema             int  `json:"schema"`
	RecoveryAttempted  bool `json:"recovery_attempted,omitempty"`
	RecoverySuppressed bool `json:"recovery_suppressed,omitempty"`
}

func cursorState(v model.Session) CursorListenerState {
	var state CursorListenerState
	_ = json.Unmarshal(v.AdapterState, &state)
	return state
}

func setCursorState(v *model.Session, state CursorListenerState) {
	state.Schema = 1
	v.AdapterState, _ = json.Marshal(state)
}

func (stubCursorAdapter) DecodeState(in agent.StateInput) (json.RawMessage, error) {
	if len(in.State) == 0 {
		v := model.Session{}
		setCursorState(&v, CursorListenerState{})
		return v.AdapterState, nil
	}
	var state CursorListenerState
	if err := json.Unmarshal(in.State, &state); err != nil {
		return nil, err
	}
	if state.Schema != 1 {
		return nil, model.NewError(model.CodeUnsupportedSchema, "unsupported Cursor state schema %d", state.Schema)
	}
	v := model.Session{}
	setCursorState(&v, state)
	return v.AdapterState, nil
}
func (stubCursorAdapter) Configuration(binary string, args []string) []model.ConfigDeclaration {
	return []model.ConfigDeclaration{{Path: ".fixture/mcp.json", Draft: "fixture-mcp.json", JSON: clientconfig.MCPConfig(binary, args, false)}}
}

// testRegistry registers dispatch stubs; real client tests live in adapters.
func testRegistry() *agent.Registry {
	r, err := agent.NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, stubCursorAdapter{})
	if err != nil {
		panic(err)
	}
	return r
}

// stubNativeFields mirror the real Cursor adapter's declared hook keys.
var stubNativeFields = []agent.NativeField{
	{Key: "conversation_id", Kind: agent.NativeString, Capture: agent.CaptureID},
	{Key: "workspace_roots", Kind: agent.NativeStrings, Capture: agent.CapturePaths},
	{Key: "status", Kind: agent.NativeString},
	{Key: "loop_count", Kind: agent.NativeInt},
}

type stubNativeInput struct {
	ConversationID string   `json:"conversation_id"`
	WorkspaceRoots []string `json:"workspace_roots"`
	Status         string   `json:"status"`
	LoopCount      int      `json:"loop_count"`
}

func stubNativeOf(v agent.HookInput) stubNativeInput {
	var n stubNativeInput
	_ = json.Unmarshal(v.Native, &n)
	return n
}
