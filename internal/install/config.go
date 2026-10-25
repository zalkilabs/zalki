package install

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigFile declares one client configuration file an adapter owns.
type ConfigFile struct {
	Path  string
	Draft string
	JSON  map[string]any
	TOML  string
	// ConfigRootEnv relocates this file's basename; Path remains its stable ownership key.
	ConfigRootEnv string
}

func (file ConfigFile) target(home string) (string, error) {
	root := os.Getenv(file.ConfigRootEnv)
	if file.ConfigRootEnv == "" || strings.TrimSpace(root) == "" {
		return filepath.Join(home, file.Path), nil
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%s must be an absolute configuration directory", file.ConfigRootEnv)
	}
	return filepath.Join(root, filepath.Base(file.Path)), nil
}

// SafePath reports whether path is a clean relative path that stays below its root.
func SafePath(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

// HookCommand builds the shell command a client runs for lifecycle hooks.
func HookCommand(binary string, args []string, kind string) string {
	parts := append([]string{binary}, args...)
	parts = append(parts, "hook", "--agent", kind)
	for i := range parts {
		parts[i] = ShellQuote(parts[i])
	}
	return strings.Join(parts, " ")
}

// HookConfig builds a hooks patch that runs command for each native event.
func HookConfig(events map[string]string, command string, flat bool) map[string]any {
	hooks := map[string]any{}
	for native, common := range events {
		handler := map[string]any{"command": command, "timeout": 5}
		var entry any
		if flat {
			if common == "Stop" {
				handler["loop_limit"] = 5
			}
			entry = handler
		} else {
			handler["type"] = "command"
			entry = map[string]any{"hooks": []any{handler}}
		}
		hooks[native] = []any{entry}
	}
	return map[string]any{"hooks": hooks}
}

// MCPConfig builds an mcpServers patch registering the zalki server.
func MCPConfig(binary string, args []string, typed bool) map[string]any {
	server := map[string]any{"command": binary, "args": append(append([]string{}, args...), "mcp")}
	if typed {
		server["type"] = "stdio"
	}
	return map[string]any{"mcpServers": map[string]any{"zalki": server}}
}

func backupStamp() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
