package bus

import (
	"context"
	"encoding/json"
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
	return configDir("CODEX_HOME", filepath.Join(home, ".codex"))
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

// testRegistry registers the real Cursor adapter and dispatch stubs for extracted clients.
func testRegistry() *Registry {
	r, err := NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, cursorAdapter{})
	if err != nil {
		panic(err)
	}
	return r
}
