package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

type claudeAdapter struct{ bus.StandardHooks }

func (a claudeAdapter) Configuration(binary string, args []string) []install.ConfigFile {
	return []install.ConfigFile{
		{Path: ".claude/settings.json", ConfigRootEnv: "CLAUDE_CONFIG_DIR", Draft: "claude-hooks.json", JSON: install.HookConfig(a.Descriptor().Hooks, install.HookCommand(binary, args, "claude"), false)},
		{Path: ".claude.json", ConfigRootEnv: "CLAUDE_CONFIG_DIR", Draft: "claude-mcp.json", JSON: install.MCPConfig(binary, args, true)},
	}
}

func (claudeAdapter) Descriptor() bus.AdapterDescriptor {
	return bus.AdapterDescriptor{Kind: "claude", Label: "Claude", Hooks: bus.StandardEvents(false), ProcessNames: []string{"claude"}, OwnerRole: "client", Executable: "claude"}
}

func (claudeAdapter) ReceiveCapabilities(bus.Session) bus.ReceiveCapabilities {
	return bus.ReceiveCapabilities{IdleWake: true, Delivery: "experimental_socket", Inbox: bus.InboxNative, BusyDelivery: bus.DeliveryBetweenTools, LifecycleHooks: true, Experimental: true, RequiresEndpoint: true}
}

func (claudeAdapter) Deliver(ctx context.Context, msg bus.Message, target bus.Session) bus.Receipt {
	return writeClaude(ctx, msg, target)
}

func (claudeAdapter) ConfigDir() string {
	home, _ := os.UserHomeDir()
	return configDir("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
}

func (a claudeAdapter) LookupName(v bus.Session, indexes map[string]map[string]string) (string, string) {
	root := v.NativeConfigDir
	if !filepath.IsAbs(root) {
		root = a.ConfigDir()
	}
	index := "claude:" + root
	if _, ok := indexes[index]; !ok {
		indexes[index] = claudeNames(root)
	}
	return indexes[index][bus.NativeID(v)], "claude_session_metadata"
}

func (claudeAdapter) RegisterHook(event bus.HookInput) (bus.HookRegistration, error) {
	if value := os.Getenv("CLAUDE_CODE_SESSION_ID"); value != "" && !strings.EqualFold(value, event.SessionID) {
		return bus.HookRegistration{}, errors.New("hook session/environment identity mismatch")
	}
	endpoint, err := bus.InspectEndpoint(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"))
	return bus.HookRegistration{Endpoint: endpoint}, err
}

func (claudeAdapter) Health(_ bus.HealthHost, v bus.Session) (string, string) {
	if bus.CheckEndpoint(v.Endpoint) == nil {
		return "bound_socket_present", ""
	}
	return "socket_unavailable", ""
}

func writeClaude(ctx context.Context, msg bus.Message, target bus.Session) bus.Receipt {
	if err := bus.CheckEndpoint(target.Endpoint); err != nil {
		return failedReceipt(bus.StatusNotSent, err)
	}
	// This undocumented wire format may change between client versions.
	frame := map[string]any{"msgV": 1, "msg_id": msg.ID, "type": "user",
		"message": map[string]string{"role": "user", "content": bus.Envelope(msg)}, "priority": "next"}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return failedReceipt(bus.StatusNotSent, err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > 65536 {
		receipt := bus.NewReceipt(bus.StatusNotSent)
		receipt.Error = "Claude frame too large"
		return receipt
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", target.Endpoint.Path)
	if err != nil {
		return failedReceipt(bus.StatusNotSent, err)
	}
	defer conn.Close() //nolint:errcheck // Delivery reports write errors before connection cleanup.
	if err := bus.CheckEndpoint(target.Endpoint); err != nil {
		return failedReceipt(bus.StatusNotSent, err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return failedReceipt(bus.StatusNotSent, err)
	}
	for len(encoded) > 0 {
		n, err := conn.Write(encoded)
		if err != nil || n == 0 {
			if err == nil {
				err = errors.New("zero byte write")
			}
			return failedReceipt(bus.StatusUnknownWrite, err)
		}
		encoded = encoded[n:]
	}
	return bus.NewReceipt(bus.StatusTransportWritten)
}

func claudeNames(root string) map[string]string {
	names := map[string]string{}
	conflicts := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(root, "sessions", "*.json"))
	for _, path := range files {
		var row struct {
			ID   string `json:"sessionId"`
			Name string `json:"name"`
		}
		if readClaudeMetadata(path, &row) != nil || !validClaudeName(row.Name) {
			continue
		}
		if previous, ok := names[row.ID]; ok && previous != row.Name {
			conflicts[row.ID] = true
		}
		names[row.ID] = row.Name
	}
	for id := range conflicts {
		delete(names, id)
	}
	return names
}

// Claude metadata has the same 128 KiB bound as the former journal reader, but
// is client data rather than a journal record. Reject symlinks and trailing JSON.
func readClaudeMetadata(path string, target any) error {
	const limit = 128 * 1024
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("invalid Claude metadata file")
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is under the recipient's Claude profile.
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // Read-only metadata lookup has no pending writes.
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("Claude metadata file too large")
	}
	return json.Unmarshal(data, target)
}

// Filter before conflict detection; an invalid label must not hide a valid one.
// Core also validates labels returned by adapters before exposing them.
func validClaudeName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && len(name) <= 512 && utf8.ValidString(name) &&
		!strings.ContainsFunc(name, unicode.IsControl)
}
