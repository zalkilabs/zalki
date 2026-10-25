package bus

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/install"
)

type claudeAdapter struct{ standardHooks }

func (a claudeAdapter) Configuration(binary string, args []string) []install.ConfigFile {
	return []install.ConfigFile{
		{Path: ".claude/settings.json", ConfigRootEnv: "CLAUDE_CONFIG_DIR", Draft: "claude-hooks.json", JSON: install.HookConfig(a.Descriptor().Hooks, install.HookCommand(binary, args, "claude"), false)},
		{Path: ".claude.json", ConfigRootEnv: "CLAUDE_CONFIG_DIR", Draft: "claude-mcp.json", JSON: install.MCPConfig(binary, args, true)},
	}
}

func (claudeAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "claude", Label: "Claude", Hooks: standardEvents(false), ProcessNames: []string{"claude"}, OwnerRole: "client", Executable: "claude"}
}

func (claudeAdapter) ReceiveCapabilities(Session) ReceiveCapabilities {
	return ReceiveCapabilities{IdleWake: true, Delivery: "experimental_socket", Inbox: InboxNative, BusyDelivery: DeliveryBetweenTools, LifecycleHooks: true, Experimental: true, RequiresEndpoint: true}
}

func (claudeAdapter) Deliver(ctx context.Context, msg Message, target Session) Receipt {
	return writeClaude(ctx, msg, target)
}

func (claudeAdapter) ConfigDir() string {
	home, _ := os.UserHomeDir()
	return configDir("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
}

func (a claudeAdapter) LookupName(v Session, indexes map[string]map[string]string) (string, string) {
	root := v.NativeConfigDir
	if !filepath.IsAbs(root) {
		root = a.ConfigDir()
	}
	index := "claude:" + root
	if _, ok := indexes[index]; !ok {
		indexes[index] = claudeNames(root)
	}
	return indexes[index][nativeID(v)], "claude_session_metadata"
}

func (claudeAdapter) RegisterHook(event HookInput) (hookRegistration, error) {
	if value := os.Getenv("CLAUDE_CODE_SESSION_ID"); value != "" && !strings.EqualFold(value, event.SessionID) {
		return hookRegistration{}, errors.New("hook session/environment identity mismatch")
	}
	endpoint, err := InspectEndpoint(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"))
	return hookRegistration{Endpoint: endpoint}, err
}

func (claudeAdapter) Health(_ HealthHost, v Session) (string, string) {
	if CheckEndpoint(v.Endpoint) == nil {
		return "bound_socket_present", ""
	}
	return "socket_unavailable", ""
}

func writeClaude(ctx context.Context, msg Message, target Session) Receipt {
	if err := CheckEndpoint(target.Endpoint); err != nil {
		return Receipt{At: timestamp(), Status: StatusNotSent, Error: err.Error(), cause: err}
	}
	// This undocumented wire format may change between client versions.
	frame := map[string]any{"msgV": 1, "msg_id": msg.ID, "type": "user",
		"message": map[string]string{"role": "user", "content": envelope(msg)}, "priority": "next"}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return Receipt{At: timestamp(), Status: StatusNotSent, Error: err.Error(), cause: err}
	}
	encoded = append(encoded, '\n')
	if len(encoded) > 65536 {
		return Receipt{At: timestamp(), Status: StatusNotSent, Error: "Claude frame too large"}
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", target.Endpoint.Path)
	if err != nil {
		return Receipt{At: timestamp(), Status: StatusNotSent, Error: err.Error(), cause: err}
	}
	defer conn.Close()
	if err := CheckEndpoint(target.Endpoint); err != nil {
		return Receipt{At: timestamp(), Status: StatusNotSent, Error: err.Error(), cause: err}
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return Receipt{At: timestamp(), Status: StatusNotSent, Error: err.Error(), cause: err}
	}
	for len(encoded) > 0 {
		n, err := conn.Write(encoded)
		if err != nil || n == 0 {
			if err == nil {
				err = errors.New("zero byte write")
			}
			return Receipt{At: timestamp(), Status: StatusUnknownWrite, Error: err.Error(), cause: err}
		}
		encoded = encoded[n:]
	}
	return Receipt{At: timestamp(), Status: StatusTransportWritten}
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
		if readJSON(path, &row) != nil || !validName(row.Name) {
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
