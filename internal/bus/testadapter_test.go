package bus

import (
	"context"
	"encoding/json"
	"io"

	"github.com/zalkilabs/zalki/internal/install"
)

// stubCodexAdapter stands in for the real Codex adapter, which lives in internal/adapters
// and cannot be imported here without an import cycle. It supplies dispatch
// metadata only; lifecycle policy is tested with Builtin in internal/adapters.
type stubCodexAdapter struct{ StandardHooks }

func (stubCodexAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "codex", Label: "Codex", Hooks: StandardEvents(true), ProcessNames: []string{"codex"}, OwnerRole: "backend", Executable: "codex"}
}

func (stubCodexAdapter) ReceiveCapabilities(Session) ReceiveCapabilities {
	return ReceiveCapabilities{IdleWake: true, Delivery: "native_queue_next_turn", Inbox: InboxNative, BusyDelivery: DeliveryNextTurn, LifecycleHooks: true}
}

func (stubCodexAdapter) Deliver(context.Context, Message, Session) Receipt {
	return NewReceipt(StatusSubmitted)
}

func (stubCodexAdapter) Health(HealthHost, Session) (string, string) { return "native_queue_only", "" }

// stubClaudeAdapter provides the native-inbox dispatch contract for core tests.
// Real socket, metadata, configuration and hook registration tests live in adapters.
type stubClaudeAdapter struct{ StandardHooks }

func (stubClaudeAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "claude", Label: "Claude", Hooks: StandardEvents(false), ProcessNames: []string{"claude"}, OwnerRole: "client", Executable: "claude"}
}

func (stubClaudeAdapter) ReceiveCapabilities(Session) ReceiveCapabilities {
	return ReceiveCapabilities{IdleWake: true, Delivery: "experimental_socket", Inbox: InboxNative, BusyDelivery: DeliveryBetweenTools, LifecycleHooks: true, Experimental: true, RequiresEndpoint: true}
}

func (stubClaudeAdapter) Deliver(context.Context, Message, Session) Receipt {
	panic("core Claude stub requires an explicit test transport")
}

// stubCursorAdapter supplies only the core dispatch/lifecycle contract. Socket
// sender, metadata, configuration and output-policy tests use the real Builtin.
type stubCursorAdapter struct{ StandardHooks }

func (stubCursorAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "cursor", Label: "Cursor", ProcessNames: []string{"agent", "cursor-agent"}, OwnerRole: "client", Executable: "agent", Hooks: map[string]string{"sessionStart": "SessionStart", "sessionEnd": "SessionEnd", "beforeSubmitPrompt": "UserPromptSubmit", "postToolUse": "PostToolUse", "stop": "Stop"}}
}
func (stubCursorAdapter) ReceiveCapabilities(v Session) ReceiveCapabilities {
	c := ReceiveCapabilities{Delivery: "next_postToolUse_or_completed_stop_hook", Inbox: InboxJournal, BusyDelivery: DeliveryNextHook, LifecycleHooks: true, HookDelivery: true, RequiresListener: true}
	if cursorState(v).Wake != nil {
		c.IdleWake, c.Delivery, c.BusyDelivery = true, "native_background_shell_output", DeliveryNotification
	}
	return c
}
func (stubCursorAdapter) Deliver(_ context.Context, _ Message, v Session) Receipt {
	if cursorState(v).Wake != nil {
		panic("core Cursor stub cannot simulate native wake acceptance")
	}
	return NewReceipt(StatusPendingActivity)
}
func (stubCursorAdapter) NormalizeHook(v HookInput) (HookInput, error) {
	if v.SessionID == "" {
		v.SessionID = v.ConversationID
	}
	if v.CWD == "" && len(v.WorkspaceRoots) > 0 {
		v.CWD = v.WorkspaceRoots[0]
	}
	return v, nil
}
func (stubCursorAdapter) HandleHook(host HookHost, v HookInput, id string, _ io.Writer) (bool, error) {
	h := host.(interface {
		CursorPrompt(string) error
		SuppressCursorRecovery(string) error
	})
	if v.Event == "UserPromptSubmit" {
		return true, h.CursorPrompt(id)
	}
	if v.Event == "Stop" && (v.Status != "completed" || v.LoopCount >= 5) {
		return true, h.SuppressCursorRecovery(id)
	}
	return false, nil
}
func (stubCursorAdapter) StartupOutput(v StartupContext) (any, error) {
	return map[string]any{"additional_context": v.Instructions, "env": map[string]string{"ZALKI_ACTOR": v.Actor, "ZALKI_CAPABILITY": v.Capability}}, nil
}
func (stubCursorAdapter) DecodeState(v *Session, raw json.RawMessage) error {
	return (CursorListenerState{}).Restore(v, raw)
}
func (stubCursorAdapter) Configuration(binary string, args []string) []install.ConfigFile {
	return []install.ConfigFile{{Path: ".fixture/mcp.json", Draft: "fixture-mcp.json", JSON: install.MCPConfig(binary, args, false)}}
}

// testRegistry registers dispatch stubs; real client tests live in adapters.
func testRegistry() *Registry {
	r, err := NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, stubCursorAdapter{})
	if err != nil {
		panic(err)
	}
	return r
}
