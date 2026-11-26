// Package clientconfig builds the native client configuration patches that
// Zalki declares (hook commands, hook entries and the MCP server entry) and
// the shell quoting they need. It also selects the native profile folder of a
// client. It depends on the standard library only, so client adapters can use
// it without importing the installer.
package clientconfig

import "strings"

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

// ShellQuote single-quotes value for safe use in a POSIX shell command.
func ShellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
