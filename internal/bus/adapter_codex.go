package bus

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
	"time"
)

type codexAdapter struct{ standardHooks }

func (a codexAdapter) Configuration(binary string, args []string) []ConfigFile {
	command, _ := json.Marshal(binary)
	arguments, _ := json.Marshal(append(append([]string{}, args...), "mcp"))
	return []ConfigFile{
		{Path: ".codex/hooks.json", ConfigRootEnv: "CODEX_HOME", Draft: "codex-hooks.json", JSON: hookConfig(a.Descriptor().Hooks, hookCommand(binary, args, "codex"), false)},
		{Path: ".codex/config.toml", ConfigRootEnv: "CODEX_HOME", Draft: "codex-mcp.toml", TOML: fmt.Sprintf("\n[mcp_servers.zalki]\ncommand = %s\nargs = %s\n", command, arguments)},
	}
}

func (codexAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "codex", Label: "Codex", Hooks: standardEvents(true), ProcessNames: []string{"codex"}, OwnerRole: "backend", Executable: "codex", Notes: []string{
		"A Codex backend can outlive its terminal.",
		"Codex 0.164.0 or newer can receive tool output on a loaded thread without resuming or steering it. Between-tool delivery is advertised only after this process has verified that app-server. Otherwise the message waits on codex queue. An unconfirmed or ambiguous write is not retried.",
	}}
}

func (codexAdapter) ReceiveCapabilities(v Session) ReceiveCapabilities {
	c := ReceiveCapabilities{IdleWake: true, Delivery: "native_queue_next_turn", Inbox: InboxNative, BusyDelivery: DeliveryNextTurn, LifecycleHooks: true}
	root := codexConfigRoot(v)
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		return c
	}
	switch cachedCodexSupport(endpoint, root) {
	case codexSupportVerified:
		c.Delivery = "app_server_tool_output"
		c.BusyDelivery = DeliveryBetweenTools
	case codexSupportUnavailable:
		return c
	default:
		c.Delivery = "app_server_unverified_or_queue"
	}
	return c
}

func (codexAdapter) Deliver(ctx context.Context, msg Message, target Session) Receipt {
	return deliverCodex(ctx, msg, target)
}

func (codexAdapter) ConfigDir() string {
	home, _ := os.UserHomeDir()
	return configDir("CODEX_HOME", filepath.Join(home, ".codex"))
}

func (a codexAdapter) LookupName(v Session, indexes map[string]map[string]string) (string, string) {
	root := v.NativeConfigDir
	if !filepath.IsAbs(root) {
		root = a.ConfigDir()
	}
	index := "codex:" + root
	if _, ok := indexes[index]; !ok {
		indexes[index] = codexNames(root)
	}
	return indexes[index][nativeID(v)], "codex_session_index"
}

func (codexAdapter) RegisterHook(event HookInput) (hookRegistration, error) {
	return hookRegistration{PreserveGeneration: event.Source == "compact"}, nil
}

func (codexAdapter) HandleHook(s HookHost, event HookInput, id string, output io.Writer) (bool, error) {
	if event.Event != "UserPromptSubmit" {
		return false, nil
	}
	reason, err := s.CompletedNativePrompt(id, event.Prompt)
	if err != nil || reason == "" {
		return false, err
	}
	return true, json.NewEncoder(output).Encode(map[string]bool{"continue": false})
}

func (codexAdapter) Health(_ HealthHost, v Session) (string, string) {
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

func queueCodex(ctx context.Context, msg Message, target Session) Receipt {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id := nativeID(target)
	if _, err := UUID(id); err != nil {
		return Receipt{At: timestamp(), Status: StatusNotSent, Error: err.Error(), cause: err}
	}
	command := exec.CommandContext(ctx, "codex", "queue", "--thread", id, "--message", envelope(msg))
	if filepath.IsAbs(target.NativeConfigDir) {
		command.Env = append(os.Environ(), "CODEX_HOME="+target.NativeConfigDir)
	}
	output, err := command.CombinedOutput()
	receipt := Receipt{At: timestamp(), Status: StatusSubmitted, Output: string(output)}
	if len(receipt.Output) > 8192 {
		receipt.Output = receipt.Output[:8192] + " [truncated]"
	}
	if ctx.Err() != nil {
		receipt.Status = StatusUnknownTimeout
		receipt.Error = ctx.Err().Error()
		receipt.cause = ctx.Err()
	} else if err != nil {
		receipt.Status = StatusCommandFailed
		if errors.Is(err, exec.ErrNotFound) {
			receipt.Status = StatusNotSent
		}
		receipt.Error = err.Error()
		receipt.cause = err
	}
	return receipt
}

func codexNames(root string) map[string]string {
	path := filepath.Join(root, "session_index.jsonl")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8*1024*1024 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
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
