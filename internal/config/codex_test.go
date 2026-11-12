package config

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestCodexQueueFallbackValues(t *testing.T) {
	for _, test := range []struct {
		name, raw, source string
		want              bool
	}{
		{"missing_file", "", "default", false},
		{"empty_file", `{}`, "default", false},
		{"empty_group", `{"codex":{}}`, "default", false},
		{"false", `{"codex":{"allow_queue_fallback":false}}`, "file", false},
		{"true", `{"codex":{"allow_queue_fallback":true}}`, "file", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]string{
				"ZALKI_CODEX_ALLOW_QUEUE_FALLBACK": strconv.FormatBool(!test.want),
				"ZALKI_ALLOW_QUEUE_FALLBACK":       strconv.FormatBool(!test.want),
			}
			input := fixtureInput(t, env)
			input.Flags["codex-allow-queue-fallback"] = strconv.FormatBool(!test.want)
			input.Flags["allow-queue-fallback"] = strconv.FormatBool(!test.want)
			if test.raw != "" {
				fixtureFile(t, input, test.raw)
			}
			snapshot, err := Load(input)
			value := snapshot.Settings.CodexAllowQueueFallback
			if err != nil || value.Value != test.want || value.Source != test.source {
				t.Fatalf("queue fallback value/source: %+v, %v", value, err)
			}
			origin := "default codex.allow_queue_fallback"
			if test.source == "file" {
				origin = snapshot.Paths.ConfigFile.Value + "#codex.allow_queue_fallback"
			}
			if value.Origin != origin {
				t.Fatalf("queue fallback origin: %q, want %q", value.Origin, origin)
			}
		})
	}
}

func TestCodexQueueFallbackStrictness(t *testing.T) {
	for _, raw := range []string{`null`, `"true"`, `1`, `[]`, `{}`} {
		t.Run(raw, func(t *testing.T) {
			input := fixtureInput(t, nil)
			path := fixtureFile(t, input, "{\n  \"codex\": {\n    \"allow_queue_fallback\": "+raw+"\n  }\n}")
			_, err := Load(input)
			want := path + " line 3, key \"codex.allow_queue_fallback\":"
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "Use true or false for this key, or remove the optional key to inherit its default") {
				t.Fatalf("strict bool repair: %v", err)
			}
		})
	}
}

func TestCodexQueueFallbackObjectRules(t *testing.T) {
	for _, test := range []struct{ name, raw, problem string }{
		{"duplicate", `{"codex":{"allow_queue_fallback":false,"allow_queue_fallback":true}}`, "duplicate key"},
		{"escaped_duplicate", `{"codex":{"allow_queue_fallback":false,"allow_queue_\u0066allback":true}}`, "duplicate key"},
		{"unknown", `{"codex":{"queue":true}}`, "Allowed keys are allow_queue_fallback"},
		{"null_group", `{"codex":null}`, "Use a JSON object for this key"},
		{"bool_group", `{"codex":true}`, "Use a JSON object for this key"},
		{"case_group", `{"Codex":{"allow_queue_fallback":true}}`, "unknown key"},
		{"case_leaf", `{"codex":{"Allow_queue_fallback":true}}`, "unknown key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := fixtureInput(t, nil)
			fixtureFile(t, input, test.raw)
			if _, err := Load(input); err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("strict Codex object: %v", err)
			}
		})
	}
}

func TestCodexQueueFallbackSnapshot(t *testing.T) {
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, `{"codex":{"allow_queue_fallback":true}}`)
	first, err := Load(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"codex":{"allow_queue_fallback":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := Load(input)
	if err != nil || !first.Settings.CodexAllowQueueFallback.Value || second.Settings.CodexAllowQueueFallback.Value {
		t.Fatal("queue policy did not stay in its invocation snapshot", err)
	}
}
