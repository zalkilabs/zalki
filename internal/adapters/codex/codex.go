package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/model"
)

// Adapter implements the Codex client.
type Adapter struct{ agent.StandardHooks }

// New returns the Codex adapter.
func New() Adapter { return Adapter{} }

// Configuration declares the Codex hook and MCP files. Hook patches use
// install.HookConfig and install.HookCommand; the MCP declaration uses TOML.
// Clients may depend on those helpers, but the file specification belongs to
// the installer, so the result is a model.ConfigDeclaration.
func (a Adapter) Configuration(binary string, args []string) []model.ConfigDeclaration {
	command, _ := json.Marshal(binary)
	arguments, _ := json.Marshal(append(append([]string{}, args...), "mcp"))
	return []model.ConfigDeclaration{
		{Path: ".codex/hooks.json", ConfigRootEnv: "CODEX_HOME", Draft: "codex-hooks.json", JSON: install.HookConfig(a.Descriptor().Hooks, install.HookCommand(binary, args, "codex"), false)},
		{Path: ".codex/config.toml", ConfigRootEnv: "CODEX_HOME", Draft: "codex-mcp.toml", TOML: fmt.Sprintf("\n[mcp_servers.zalki]\ncommand = %s\nargs = %s\n", command, arguments)},
	}
}

func (Adapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "codex", Label: "Codex", Hooks: agent.StandardEvents(true), ProcessNames: []string{"codex"}, OwnerRole: "backend", Executable: "codex", Notes: []string{
		"A Codex backend can outlive its terminal.",
		"Codex 0.164.0 or newer can receive tool output on a loaded thread without resuming or steering it. Between-tool delivery is advertised only after this process has verified that app-server. Otherwise the message waits on codex queue. An unconfirmed or ambiguous write is not retried.",
	}}
}

func (Adapter) ReceiveCapabilities(v bus.Session) model.ReceiveCapabilities {
	c := model.ReceiveCapabilities{IdleWake: true, Delivery: "native_queue_next_turn", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn, LifecycleHooks: true}
	root := codexConfigRoot(v)
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		return c
	}
	switch cachedCodexSupport(endpoint, root) {
	case codexSupportVerified:
		c.Delivery = "app_server_tool_output"
		c.BusyDelivery = model.DeliveryBetweenTools
	case codexSupportUnavailable:
		return c
	default:
		c.Delivery = "app_server_unverified_or_queue"
	}
	return c
}

func (Adapter) Deliver(ctx context.Context, msg bus.Message, target bus.Session) bus.Receipt {
	return deliverCodex(ctx, msg, target)
}

func (Adapter) ConfigDir() string {
	home, _ := os.UserHomeDir()
	return configDir("CODEX_HOME", filepath.Join(home, ".codex"))
}

func (a Adapter) LookupName(v bus.Session, indexes map[string]map[string]string) (string, string) {
	root := v.NativeConfigDir
	if !filepath.IsAbs(root) {
		root = a.ConfigDir()
	}
	index := "codex:" + root
	if _, ok := indexes[index]; !ok {
		indexes[index] = codexNames(root)
	}
	return indexes[index][bus.NativeID(v)], "codex_session_index"
}

func (Adapter) RegisterHook(event agent.HookInput) (agent.HookRegistration, error) {
	return agent.HookRegistration{PreserveGeneration: event.Source == "compact"}, nil
}

func (Adapter) HandleHook(s agent.HookHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	if event.Event != "UserPromptSubmit" {
		return false, nil
	}
	reason, err := s.CompletedNativePrompt(id, event.Prompt)
	if err != nil || reason == "" {
		return false, err
	}
	return true, json.NewEncoder(output).Encode(map[string]bool{"continue": false})
}

func (Adapter) Health(_ agent.HealthHost, v bus.Session) (string, string) {
	root := codexConfigRoot(v)
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		return "native_queue_only", ""
	}
	switch cachedCodexSupport(endpoint, root) {
	case codexSupportVerified:
		return "app_server_tool_output", ""
	case codexSupportUnavailable:
		return "native_queue_only", ""
	default:
		return "app_server_socket_present", ""
	}
}

func queueCodex(ctx context.Context, msg bus.Message, target bus.Session) bus.Receipt {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id := bus.NativeID(target)
	if _, err := bus.UUID(id); err != nil {
		return failedReceipt(bus.StatusNotSent, err)
	}
	command := exec.CommandContext(ctx, "codex", //nolint:gosec // G204: fixed executable; thread id is a validated bus.UUID
		"queue", "--thread", id, "--message", bus.Envelope(msg))
	if filepath.IsAbs(target.NativeConfigDir) {
		command.Env = append(os.Environ(), "CODEX_HOME="+target.NativeConfigDir)
	}
	output, err := command.CombinedOutput()
	receipt := bus.NewReceipt(bus.StatusSubmitted)
	receipt.Output = string(output)
	if len(receipt.Output) > 8192 {
		receipt.Output = receipt.Output[:8192] + " [truncated]"
	}
	if ctx.Err() != nil {
		receipt.Status = bus.StatusUnknownTimeout
		receipt.Error = ctx.Err().Error()
		receipt = receipt.WithCause(ctx.Err())
	} else if err != nil {
		receipt.Status = bus.StatusCommandFailed
		if errors.Is(err, exec.ErrNotFound) {
			receipt.Status = bus.StatusNotSent
		}
		receipt.Error = err.Error()
		receipt = receipt.WithCause(err)
	}
	return receipt
}

func codexNames(root string) map[string]string {
	path := filepath.Join(root, "session_index.jsonl")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8*1024*1024 {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is under the recipient's Codex profile
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck // Read-only name lookup has no pending writes.
	names := map[string]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 128*1024)
	for scanner.Scan() {
		var row struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) == nil {
			// Appended updates supersede previous names, including empty names.
			names[row.ID] = row.Name
		} else {
			return nil // Do not resurrect old labels from a partially written index.
		}
	}
	if scanner.Err() != nil {
		return nil
	}
	return names
}

func configDir(variable, fallback string) string {
	if value := os.Getenv(variable); strings.TrimSpace(value) != "" {
		if path, err := filepath.Abs(value); err == nil {
			return path
		}
	}
	return fallback
}

func failedReceipt(status bus.DeliveryStatus, err error) bus.Receipt {
	receipt := bus.NewReceipt(status)
	receipt.Error = err.Error()
	return receipt.WithCause(err)
}
