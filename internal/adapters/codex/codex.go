// Package codex implements the Codex adapter: hook normalization, app-server
// delivery, client configuration and client-specific liveness policy.
// Core messaging and journal policy remain in bus.
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
	"strconv"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/model"
)

// Adapter implements the Codex client.
type Adapter struct {
	agent.StandardHooks
	allowQueueFallback bool
	configFile         string
}

// New returns the Codex adapter.
func New(allow bool, configFile string) Adapter {
	return Adapter{allowQueueFallback: allow, configFile: configFile}
}

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
		"Ended Codex threads stay listed until zalki prune --session codex:UUID.",
		"Codex 0.164.0 or newer can receive tool output on a loaded thread without resuming or steering it. Between-tool delivery is advertised only after this process has verified that app-server. Otherwise delivery requires the owner to enable codex.allow_queue_fallback; queue passes message text in process arguments. An unconfirmed or ambiguous write is not retried.",
	}}
}

func (a Adapter) ReceiveCapabilities(v model.Session) model.ReceiveCapabilities {
	c := model.ReceiveCapabilities{Delivery: "app_server_queue_off_config", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn, LifecycleHooks: true}
	if a.allowQueueFallback {
		c.IdleWake = true
		c.Delivery = "native_queue_next_turn"
		c.BusyDelivery = model.DeliveryNextTurn
	}
	root := codexConfigRoot(v)
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		return c
	}
	switch cachedCodexSupport(endpoint, root) {
	case codexSupportVerified:
		c.IdleWake = true
		c.Delivery = "app_server_tool_output"
		c.BusyDelivery = model.DeliveryBetweenTools
	case codexSupportUnavailable:
		return c
	default:
		if a.allowQueueFallback {
			c.Delivery = "app_server_unverified_or_queue"
		} else {
			c.Delivery = "app_server_unverified_queue_off_config"
		}
	}
	return c
}

func (a Adapter) Deliver(ctx context.Context, msg model.Message, target model.Session) model.Receipt {
	return a.deliverCodex(ctx, msg, target)
}

func (Adapter) ConfigDir() string {
	home, _ := os.UserHomeDir()
	return configDir("CODEX_HOME", filepath.Join(home, ".codex"))
}

func (a Adapter) LookupName(v model.Session, indexes map[string]map[string]string) (string, string) {
	root := v.NativeConfigDir
	if !filepath.IsAbs(root) {
		root = a.ConfigDir()
	}
	index := "codex:" + root
	if _, ok := indexes[index]; !ok {
		indexes[index] = codexNames(root)
	}
	return indexes[index][v.NativeID], "codex_session_index"
}

func (Adapter) RegisterHook(event agent.HookInput) (agent.HookRegistration, error) {
	return agent.HookRegistration{PreserveGeneration: event.Source == "compact"}, nil
}

func (Adapter) HandleHook(s agent.HookHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	// SessionEnd also covers unloading; its reason does not prove user closure.
	if event.Event == "SessionEnd" {
		return true, s.UpdateSession(id, model.StateReady, false)
	}
	if event.Event != "UserPromptSubmit" {
		return false, nil
	}
	reason, err := s.CompletedNativePrompt(id, event.Prompt)
	if err != nil || reason == "" {
		return false, err
	}
	return true, json.NewEncoder(output).Encode(map[string]bool{"continue": false})
}

func (a Adapter) Health(_ agent.HealthHost, v model.Session) (string, string) {
	root := codexConfigRoot(v)
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		if a.allowQueueFallback {
			return "native_queue_only", ""
		}
		return "app_server_required_queue_off_config", ""
	}
	switch cachedCodexSupport(endpoint, root) {
	case codexSupportVerified:
		return "app_server_tool_output", ""
	case codexSupportUnavailable:
		if a.allowQueueFallback {
			return "native_queue_only", ""
		}
		return "app_server_required_queue_off_config", ""
	default:
		if !a.allowQueueFallback {
			return "app_server_socket_present_queue_off_config", ""
		}
		return "app_server_socket_present", ""
	}
}

func queueCodex(ctx context.Context, msg model.Message, target model.Session) model.Receipt {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id := target.NativeID
	if _, err := model.UUID(id); err != nil {
		return failedReceipt(model.StatusNotSent, err)
	}
	command := exec.CommandContext(ctx, "codex", //nolint:gosec // G204: fixed executable; thread id is a validated bus.UUID
		"queue", "--thread", id, "--message", model.Envelope(msg))
	if filepath.IsAbs(target.NativeConfigDir) {
		command.Env = append(os.Environ(), "CODEX_HOME="+target.NativeConfigDir)
	}
	output, err := command.CombinedOutput()
	receipt := model.NewReceipt(model.StatusSubmitted)
	receipt.Output = string(output)
	if len(receipt.Output) > 8192 {
		receipt.Output = receipt.Output[:8192] + " [truncated]"
	}
	if ctx.Err() != nil {
		receipt.Status = model.StatusUnknownTimeout
		receipt.Error = ctx.Err().Error()
		receipt = receipt.WithCause(ctx.Err())
	} else if err != nil {
		receipt.Status = model.StatusCommandFailed
		if errors.Is(err, exec.ErrNotFound) {
			receipt.Status = model.StatusNotSent
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

func failedReceipt(status model.DeliveryStatus, err error) model.Receipt {
	receipt := model.NewReceipt(status)
	receipt.Error = err.Error()
	return receipt.WithCause(err)
}

// queueFallbackRefusal describes the owner-only exception without exposing a raw path.
func (a Adapter) queueFallbackRefusal() model.Receipt {
	path := a.configFile
	if path == "" {
		path = "the selected Zalki config file"
	}
	if runes := []rune(path); len(runes) > 256 {
		path = string(runes[:256]) + "…"
	}
	err := model.NewError(model.CodeUnavailable, "%s", "Codex app-server delivery is unavailable for this session and the queue fallback is off. The owner can set codex.allow_queue_fallback=true in "+strconv.QuoteToASCII(path)+"; this exposes message text to other local users in process listings (up to 30 s). Agents must not change this setting.")
	return failedReceipt(model.StatusNotSent, err)
}
