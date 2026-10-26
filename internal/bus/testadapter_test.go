package bus

import (
	"context"
	"encoding/json"
	"github.com/zalkilabs/zalki/internal/install"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// stubCodexAdapter stands in for the real Codex adapter, which lives in internal/adapters
// and cannot be imported here without an import cycle. It mirrors the contract
// core relies on: kind, label, hook events, native inbox, and prompt filtering.
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

func (stubCodexAdapter) ConfigDir() string {
	home, _ := os.UserHomeDir()
	return stubConfigDir("CODEX_HOME", filepath.Join(home, ".codex"))
}

// LookupName reads Codex's append-only session_index.jsonl: the last row for an id wins,
// and a partially written row discards the whole index. The real parser is tested in internal/adapters.
func (a stubCodexAdapter) LookupName(v Session, indexes map[string]map[string]string) (string, string) {
	root := v.NativeConfigDir
	if !filepath.IsAbs(root) {
		root = a.ConfigDir()
	}
	index := "codex:" + root
	if _, ok := indexes[index]; !ok {
		indexes[index] = stubCodexNames(root)
	}
	return indexes[index][NativeID(v)], "codex_session_index"
}

func stubCodexNames(root string) map[string]string {
	data, err := os.ReadFile(filepath.Join(root, "session_index.jsonl"))
	if err != nil {
		return nil
	}
	names := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal([]byte(line), &row) != nil {
			return nil
		}
		names[row.ID] = row.Name
	}
	return names
}

func (stubCodexAdapter) RegisterHook(event HookInput) (HookRegistration, error) {
	return HookRegistration{PreserveGeneration: event.Source == "compact"}, nil
}

func (stubCodexAdapter) HandleHook(s HookHost, event HookInput, id string, output io.Writer) (bool, error) {
	if event.Event != "UserPromptSubmit" {
		return false, nil
	}
	reason, err := s.CompletedNativePrompt(id, event.Prompt)
	if err != nil || reason == "" {
		return false, err
	}
	return true, json.NewEncoder(output).Encode(map[string]bool{"continue": false})
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

func stubConfigDir(variable, fallback string) string {
	if value := os.Getenv(variable); strings.TrimSpace(value) != "" {
		if path, err := filepath.Abs(value); err == nil {
			return path
		}
	}
	return fallback
}
