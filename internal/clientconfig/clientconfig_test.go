package clientconfig_test

import (
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/clientconfig"
)

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	for value, want := range map[string]string{
		"plain":   `'plain'`,
		"it's":    `'it'"'"'s'`,
		"a b;c":   `'a b;c'`,
		"":        `''`,
		"$HOME/x": `'$HOME/x'`,
	} {
		if got := clientconfig.ShellQuote(value); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", value, got, want)
		}
	}
}

func TestHookCommandQuotesEveryPart(t *testing.T) {
	got := clientconfig.HookCommand("/opt/my zalki", []string{"--state", "s t"}, "claude")
	want := `'/opt/my zalki' '--state' 's t' 'hook' '--agent' 'claude'`
	if got != want {
		t.Fatalf("HookCommand = %s, want %s", got, want)
	}
}

func TestHookConfigShapes(t *testing.T) {
	events := map[string]string{"Stop": "Stop"}
	nested := clientconfig.HookConfig(events, "cmd", false)
	wantNested := map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"command": "cmd", "timeout": 5, "type": "command"}}}}}}
	if !reflect.DeepEqual(nested, wantNested) {
		t.Fatalf("nested hooks = %#v", nested)
	}
	flat := clientconfig.HookConfig(events, "cmd", true)
	wantFlat := map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"command": "cmd", "timeout": 5, "loop_limit": 5}}}}
	if !reflect.DeepEqual(flat, wantFlat) {
		t.Fatalf("flat hooks = %#v", flat)
	}
}

func TestMCPConfigCopiesArgsAndTypesOnRequest(t *testing.T) {
	args := []string{"--state", "s"}
	got := clientconfig.MCPConfig("/bin/zalki", args, true)
	server := got["mcpServers"].(map[string]any)["zalki"].(map[string]any)
	if want := []string{"--state", "s", "mcp"}; !reflect.DeepEqual(server["args"], want) || server["type"] != "stdio" || server["command"] != "/bin/zalki" {
		t.Fatalf("typed server = %#v", server)
	}
	if len(args) != 2 {
		t.Fatalf("input args changed: %v", args)
	}
	if _, typed := clientconfig.MCPConfig("/bin/zalki", nil, false)["mcpServers"].(map[string]any)["zalki"].(map[string]any)["type"]; typed {
		t.Fatal("untyped server has a type")
	}
}
