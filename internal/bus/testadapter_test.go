package bus

import (
	"context"
	"encoding/json"
	"io"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/model"
)

// stubCodexAdapter stands in for the real Codex adapter, which lives in internal/adapters
// and cannot be imported here without an import cycle. It supplies dispatch
// metadata only; lifecycle policy is tested with Builtin in internal/adapters.
type stubCodexAdapter struct{ agent.StandardHooks }

func (stubCodexAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "codex", Label: "Codex", Hooks: agent.StandardEvents(true), ProcessNames: []string{"codex"}, OwnerRole: "backend", Executable: "codex"}
}

func (stubCodexAdapter) ReceiveCapabilities(Session) ReceiveCapabilities {
	return ReceiveCapabilities{IdleWake: true, Delivery: "native_queue_next_turn", Inbox: InboxNative, BusyDelivery: DeliveryNextTurn, LifecycleHooks: true}
}

func (stubCodexAdapter) Deliver(context.Context, Message, Session) Receipt {
	return NewReceipt(StatusSubmitted)
}

func (stubCodexAdapter) Health(agent.HealthHost, Session) (string, string) {
	return "native_queue_only", ""
}

// stubClaudeAdapter provides the native-inbox dispatch contract for core tests.
// Real socket, metadata, configuration and hook registration tests live in adapters.
type stubClaudeAdapter struct{ agent.StandardHooks }

func (stubClaudeAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "claude", Label: "Claude", Hooks: agent.StandardEvents(false), ProcessNames: []string{"claude"}, OwnerRole: "client", Executable: "claude"}
}

func (stubClaudeAdapter) ReceiveCapabilities(Session) ReceiveCapabilities {
	return ReceiveCapabilities{IdleWake: true, Delivery: "experimental_socket", Inbox: InboxNative, BusyDelivery: DeliveryBetweenTools, LifecycleHooks: true, Experimental: true, RequiresEndpoint: true}
}

func (stubClaudeAdapter) Deliver(context.Context, Message, Session) Receipt {
	panic("core Claude stub requires an explicit test transport")
}

// stubCursorAdapter supplies only the core dispatch/lifecycle contract. Socket
// sender, metadata, configuration and output-policy tests use the real Builtin.
type stubCursorAdapter struct{ agent.StandardHooks }

func (stubCursorAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "cursor", Label: "Cursor", ProcessNames: []string{"agent", "cursor-agent"}, OwnerRole: "client", Executable: "agent", Hooks: map[string]string{"sessionStart": "SessionStart", "sessionEnd": "SessionEnd", "beforeSubmitPrompt": "UserPromptSubmit", "postToolUse": "PostToolUse", "stop": "Stop"}}
}
func (stubCursorAdapter) ReceiveCapabilities(v Session) ReceiveCapabilities {
	c := ReceiveCapabilities{Delivery: "next_postToolUse_or_completed_stop_hook", Inbox: InboxJournal, BusyDelivery: DeliveryNextHook, LifecycleHooks: true, HookDelivery: true, RequiresListener: true}
	if v.Listener != nil {
		c.IdleWake, c.Delivery, c.BusyDelivery = true, "native_background_shell_output", DeliveryNotification
	}
	return c
}
func (stubCursorAdapter) Deliver(_ context.Context, _ Message, v Session) Receipt {
	if v.Listener != nil {
		panic("core Cursor stub cannot simulate native wake acceptance")
	}
	return NewReceipt(StatusPendingActivity)
}
func (stubCursorAdapter) NormalizeHook(v agent.HookInput) (agent.HookInput, error) {
	if v.Event == "Stop" && v.Status != "" && v.Status != "completed" {
		v.Event = "Interrupt"
	}
	if v.SessionID == "" {
		v.SessionID = v.ConversationID
	}
	if v.CWD == "" && len(v.WorkspaceRoots) > 0 {
		v.CWD = v.WorkspaceRoots[0]
	}
	return v, nil
}
func (stubCursorAdapter) HandleHook(host agent.HookHost, v agent.HookInput, id string, _ io.Writer) (bool, error) {
	h := host.(agent.LifecycleHost)
	if v.Event == "UserPromptSubmit" {
		return true, h.ApplyLifecycle(stubResumeOnPrompt)
	}
	if v.Event == "Interrupt" || v.Event == "Stop" && (v.Status != "completed" || v.LoopCount >= 5) {
		return true, h.ApplyLifecycle(stubSuppressRecovery)
	}
	return false, nil
}

// The stub Cursor policies mirror the real adapter's so core transaction tests
// exercise realistic state; the adapter's own policy is tested in adapters.
func stubResumeOnPrompt(v SessionView) (agent.LifecycleChange, error) {
	state := cursorState(Session{AdapterState: v.AdapterState})
	if state.RecoverySuppressed {
		state.RecoveryAttempted = false
	}
	state.RecoverySuppressed = false
	next := Session{}
	setCursorState(&next, state)
	return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate, AdapterState: next.AdapterState}, nil
}

func stubSuppressRecovery(v SessionView) (agent.LifecycleChange, error) {
	state := cursorState(Session{AdapterState: v.AdapterState})
	state.RecoverySuppressed = true
	next := Session{}
	setCursorState(&next, state)
	return agent.LifecycleChange{Transition: agent.TransitionReady, AdapterState: next.AdapterState}, nil
}

func stubClaimRecovery(v SessionView) (agent.RecoveryDecision, error) {
	state := cursorState(Session{AdapterState: v.AdapterState})
	if state.RecoverySuppressed || state.RecoveryAttempted {
		return agent.RecoveryDecision{}, nil
	}
	state.RecoveryAttempted = true
	next := Session{}
	setCursorState(&next, state)
	return agent.RecoveryDecision{Recover: true, AdapterState: next.AdapterState}, nil
}

// claimCursorRecovery claims recovery as a Cursor postToolUse hook would.
func claimCursorRecovery(store Store, actor string) (agent.RecoveryGrant, error) {
	return hookRuntime{store: store, actor: actor, kind: "cursor", event: "PostToolUse"}.ClaimRecovery(stubClaimRecovery)
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

func cursorState(v Session) CursorListenerState {
	var state CursorListenerState
	_ = json.Unmarshal(v.AdapterState, &state)
	return state
}

func setCursorState(v *Session, state CursorListenerState) {
	state.Schema = 1
	v.AdapterState, _ = json.Marshal(state)
}

func (stubCursorAdapter) DecodeState(in agent.StateInput) (json.RawMessage, error) {
	if len(in.State) == 0 {
		var legacy struct {
			Attempted  bool `json:"cursor_recovery_attempted"`
			Suppressed bool `json:"cursor_recovery_suppressed"`
		}
		fields, _ := json.Marshal(in.Extensions)
		if err := json.Unmarshal(fields, &legacy); err != nil {
			return nil, err
		}
		v := Session{}
		setCursorState(&v, CursorListenerState{RecoveryAttempted: legacy.Attempted, RecoverySuppressed: legacy.Suppressed})
		return v.AdapterState, nil
	}
	var state CursorListenerState
	if err := json.Unmarshal(in.State, &state); err != nil {
		return nil, err
	}
	if state.Schema != 1 {
		return nil, fail(CodeUnsupportedSchema, "unsupported Cursor state schema %d", state.Schema)
	}
	v := Session{}
	setCursorState(&v, state)
	return v.AdapterState, nil
}
func (stubCursorAdapter) Configuration(binary string, args []string) []model.ConfigDeclaration {
	return []model.ConfigDeclaration{{Path: ".fixture/mcp.json", Draft: "fixture-mcp.json", JSON: install.MCPConfig(binary, args, false)}}
}

// testRegistry registers dispatch stubs; real client tests live in adapters.
func testRegistry() *Registry {
	r, err := NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, stubCursorAdapter{})
	if err != nil {
		panic(err)
	}
	return r
}

func (stubCursorAdapter) LegacyStateFields() []string {
	return []string{"cursor_wake", "cursor_recovery_attempted", "cursor_recovery_suppressed"}
}
