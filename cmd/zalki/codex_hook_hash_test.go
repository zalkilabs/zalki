package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Captured from the installed darwin-arm64 codex-cli 0.167.2 with inert,
// untrusted hooks in a disposable profile. Pinned source tree:
// ce21072c90d32c40af7a55b60b2fb35c84a43a6a (rust-v0.167.2).
func TestCodexHashNativeGoldens(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex-hook-hashes-0.167.2.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Version  string `json:"version"`
		Fixtures []struct {
			Label     string         `json:"label"`
			Event     string         `json:"event"`
			Matcher   any            `json:"matcher"`
			Handler   map[string]any `json:"handler"`
			Canonical string         `json:"canonical_json"`
			Native    string         `json:"native_hash"`
		} `json:"fixtures"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Fixtures) != 19 || corpus.Version != "0.167.2" {
		t.Fatal("D02_NATIVE_CORPUS_COUNT")
	}
	for _, fixture := range corpus.Fixtures {
		t.Run(fixture.Label, func(t *testing.T) {
			identity, err := codexHookIdentity(fixture.Event, fixture.Matcher, fixture.Handler)
			if err != nil {
				t.Fatal("D02_NATIVE_IDENTITY", err)
			}
			canonical, err := codexCanonicalJSON(nil, identity)
			if err != nil || string(canonical) != fixture.Canonical {
				t.Fatalf("D02_NATIVE_BYTES: got %q want %q (%v)", canonical, fixture.Canonical, err)
			}
			hash, err := codexHookHash(fixture.Event, fixture.Matcher, fixture.Handler)
			if err != nil || hash != fixture.Native {
				t.Fatalf("D02_NATIVE_HASH: got %s want %s (%v)", hash, fixture.Native, err)
			}
		})
	}
}

func codexTestHandler(command string) map[string]any {
	return map[string]any{"type": "command", "command": command}
}

func TestCodexHashNormalizationBoundaries(t *testing.T) {
	for _, test := range []struct {
		event string
		input any
		want  int64
	}{
		{"SessionStart", nil, 600}, {"SessionStart", int64(0), 1}, {"SessionStart", int64(900), 900},
		{"SessionEnd", nil, 1}, {"Interrupt", nil, 1}, {"SessionEnd", int64(0), 1},
		{"SessionEnd", int64(3), 3}, {"SessionEnd", int64(4), 3}, {"Interrupt", int64(100), 3},
	} {
		got, err := codexHookTimeout(test.event, test.input)
		if err != nil || got != test.want {
			t.Fatalf("D04_TIMEOUT: %s %v got %d want %d", test.event, test.input, got, test.want)
		}
	}
	for event := range codexEventKeys {
		for _, limit := range []int64{0, 1, 2500, 2501} {
			assertCodexEventNormalization(t, event, limit)
		}
	}
}

func assertCodexEventNormalization(t *testing.T, event string, limit int64) {
	t.Helper()
	handler := codexTestHandler(" cmd ")
	handler["additionalContextLimit"] = limit
	handler["async"] = true
	identity, err := codexHookIdentity(event, "  x  ", handler)
	if err != nil {
		t.Fatal(err)
	}
	normal := identity["hooks"].([]any)[0].(map[string]any)
	_, context := normal["additionalContextLimit"]
	wantContext := containsCodexField([]string{"PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", "SubagentStart"}, event) && limit != 2500
	if context != wantContext || (context && normal["additionalContextLimit"] != limit) {
		t.Fatalf("D04_CONTEXT: %s %d %#v", event, limit, normal)
	}
	if normal["async"] != true || normal["command"] != " cmd " {
		t.Fatal("D04_DECLARED_ASYNC_D03_COMMAND_BYTES")
	}
	wantMatcher := !containsCodexField([]string{"UserPromptSubmit", "Stop", "Interrupt", "PreModelRequest", "PostModelRequest"}, event)
	_, matcher := identity["matcher"]
	if matcher != wantMatcher || (matcher && identity["matcher"] != "  x  ") {
		t.Fatalf("D03_MATCHER: %s %#v", event, identity)
	}
	for _, omitted := range []string{"statusMessage", "commandWindows", "command_windows"} {
		if _, exists := normal[omitted]; exists {
			t.Fatal("D03_OMIT_OPTIONAL", omitted)
		}
	}
}

func TestCodexCanonicalBytes(t *testing.T) {
	value := map[string]any{"z": []any{true, int64(0)}, "a": map[string]any{"x": "<>&\u2028\u2029\\u2028\"\\\b\f\n\r\t\x00\x1f"}}
	want := "{\"a\":{\"x\":\"<>&\u2028\u2029\\\\u2028\\\"\\\\\\b\\f\\n\\r\\t\\u0000\\u001f\"},\"z\":[true,0]}"
	for range 100 {
		got, err := codexCanonicalJSON(nil, value)
		if err != nil || string(got) != want {
			t.Fatalf("D05_CANONICAL_BYTES: got %q want %q", got, want)
		}
	}
}

func TestCodexDefinitionRejectsUnsupportedInput(t *testing.T) {
	for _, raw := range []string{
		`{"type":"command","command":" "}`, `{"type":"prompt","command":"x"}`,
		`{"type":"command","command":"x","timeout":-1}`, `{"type":"command","command":"x","timeout":1.5}`,
		`{"type":"command","command":"x","timeout":9223372036854775808}`,
		`{"type":"command","command":"x","additionalContextLimit":-1}`,
		`{"type":"command","command":"x","async":null}`, `{"type":"command","command":"x","statusMessage":1}`,
		`{"type":"command","command":"x","commandWindows":false}`,
		`{"type":"command","command":"x","commandWindows":"a","command_windows":"b"}`,
		`{"type":"command","command":"x","unknown":true}`,
	} {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var handler map[string]any
		if err := decoder.Decode(&handler); err != nil {
			t.Fatal(err)
		}
		if _, err := codexHookHash("SessionStart", nil, handler); err == nil {
			t.Fatal("D06_UNSUPPORTED_HANDLER", raw)
		}
	}
	if _, err := codexHookHash("Unknown", nil, codexTestHandler("x")); err == nil {
		t.Fatal("D06_UNKNOWN_EVENT")
	}
	if _, err := codexHookHash("SessionStart", true, codexTestHandler("x")); err == nil {
		t.Fatal("D06_INVALID_MATCHER")
	}
}
