package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestCodexHashExactVersionGate(t *testing.T) {
	for _, version := range []string{"0.167.2", "0.167.0", "0.167.1", "0.167.20", "0.167.2-beta", "0.168.0", "", "codex-cli 0.167.2", "unknown"} {
		for _, found := range []bool{false, true} {
			want := found && version == "0.167.2"
			if got := codexHashVersionSupported(agent.DiagnosticInput{Version: version, Found: found}); got != want {
				t.Fatalf("D01_EXACT_VERSION: %q %t got %t", version, found, got)
			}
		}
	}
	if codexHashVersionSupported(agent.DiagnosticInput{}) {
		t.Fatal("D01_MISSING_CLIENT")
	}
}

func TestCodexStrictJSON(t *testing.T) {
	for _, raw := range []string{
		`{"x":1,"x":2}`, `{"x":null,"x":2}`, `{"x":{"a":1,"a":2}}`, `{} {}`,
		`{"x":9223372036854775808}`, `{"x":1.2}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`,
		`{"x":"\ud800\u0041"}`, "{\"x\":\"\xff\"}", `[]`, `{"x":`,
		`{"x":` + strings.Repeat("[", 66) + `0` + strings.Repeat("]", 66) + `}`,
	} {
		if _, err := decodeCodexJSON([]byte(raw)); err == nil {
			t.Fatalf("D06_STRICT_JSON: accepted %q", raw)
		}
	}
	for _, raw := range []string{`{}`, `{"x":"\ud83d\ude80"}`, `{"x":"\\ud800"}`, `{"x":9223372036854775807}`} {
		if _, err := decodeCodexJSON([]byte(raw)); err != nil {
			t.Fatalf("D06_VALID_JSON: rejected %q", raw)
		}
	}
}

func writeCodexFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Positive fixtures use canonical spelling; the diagnostic deliberately rejects
// symbolic parents because their spelling can change persisted source keys.
func codexDiagnosticTestDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexBoundedFilesAndTOML(t *testing.T) {
	root := codexDiagnosticTestDir(t)
	path := filepath.Join(root, "config.toml")
	if _, reason := readCodexHookStates(path); reason != "missing_file" {
		t.Fatal("D06_MISSING_FILE", reason)
	}
	for _, input := range []string{"[hooks", "x = 1\nx = 2", "[hooks.state.x]\nenabled = true\nenabled = false", "[hooks]\nstate = 1", "x = 9223372036854775808", "x = '\xff'", "[hooks.state.other]\nenabled = 'false'", "[hooks.state.other]\ntrusted_hash = 1"} {
		writeCodexFixture(t, path, input)
		if _, reason := readCodexHookStates(path); reason != "invalid_data" {
			t.Fatalf("D06_BAD_TOML: %q %q", input, reason)
		}
	}
	for _, input := range []string{"", "model = 'unrelated'", "[hooks.state]"} {
		writeCodexFixture(t, path, input)
		if _, reason := readCodexHookStates(path); reason != "" {
			t.Fatal("D06_VALID_TOML", input, reason)
		}
	}
	writeCodexFixture(t, path, "[hooks.SessionStart]\nx = 1")
	if _, reason := readCodexHookStates(path); reason != "unsupported_definition" {
		t.Fatal("D06_INLINE_UNVERIFIED", reason)
	}
	writeCodexFixture(t, path, strings.Repeat(" ", codexDiagnosticFileLimit+1))
	if _, reason := readCodexDiagnosticFile(path); reason != "unsafe_file" {
		t.Fatal("D06_LIMIT", reason)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "elsewhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, reason := readCodexDiagnosticFile(path); reason != "unsafe_file" {
		t.Fatal("D06_SYMLINK", reason)
	}
	if _, reason := readCodexDiagnosticFile(root); reason != "unsafe_file" {
		t.Fatal("D06_NONREGULAR", reason)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(root, linked); err != nil {
		t.Fatal(err)
	}
	if _, reason := readCodexDiagnosticFile(filepath.Join(linked, "config.toml")); reason != "unsafe_file" {
		t.Fatal("D06_PARENT_SYMLINK", reason)
	}
}

func codexDefinitionDocument(command string) map[string]any {
	return map[string]any{"hooks": map[string]any{"SessionStart": []any{
		map[string]any{"hooks": []any{codexTestHandler("unrelated zalki substring")}},
		map[string]any{"hooks": []any{codexTestHandler("other"), codexTestHandler(command), codexTestHandler(command)}},
	}}}
}

func TestCodexOwnedIndicesAndTrust(t *testing.T) {
	hash, err := codexHookHash("SessionStart", nil, codexTestHandler("owned"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/profile-one/hooks.json", "/profile-two/hooks.json"} {
		definitions, err := parseCodexHookDefinitions(path, codexDefinitionDocument("owned"))
		if err != nil || len(definitions) != 4 {
			t.Fatal("D03_SINGLE_HANDLER", err)
		}
		if definitions[2].hash != hash || definitions[3].hash != hash {
			t.Fatal("D03_SINGLE_HANDLER_HASH")
		}
		key := path + ":session_start:1:1"
		states := map[string]any{key: map[string]any{"trusted_hash": hash}}
		rows := codexOwnedHookRows(path, map[string]string{"SessionStart": hash, "Stop": hash}, definitions, states, "")
		if len(rows) != 3 || rows[0].Key != key || rows[0].Status != "trusted" || rows[1].Key != path+":session_start:1:2" || rows[1].Status != "not_trusted" || rows[2].Reason != "missing_hook" {
			t.Fatalf("D07_EXACT_OWNERSHIP_AND_INDICES: %#v", rows)
		}
		assertCodexReorderedStateKey(t, path, hash, states)
	}
}

func assertCodexReorderedStateKey(t *testing.T, path, hash string, states map[string]any) {
	t.Helper()
	document := codexDefinitionDocument("owned")
	groups := document["hooks"].(map[string]any)["SessionStart"].([]any)
	groups[0], groups[1] = groups[1], groups[0]
	definitions, err := parseCodexHookDefinitions(path, document)
	if err != nil {
		t.Fatal(err)
	}
	rows := codexOwnedHookRows(path, map[string]string{"SessionStart": hash}, definitions, states, "")
	if rows[0].Key != path+":session_start:0:1" || rows[0].Status != "not_trusted" {
		t.Fatal("D07_REORDER_CHANGES_STATE_KEY", rows)
	}
}

func TestCodexTrustVerdicts(t *testing.T) {
	hash, err := codexHookHash("SessionStart", nil, codexTestHandler("owned"))
	if err != nil {
		t.Fatal(err)
	}
	definition := codexHookDefinition{event: "SessionStart", key: "/a:session_start:1:1", hash: hash}
	for _, test := range []struct {
		state  any
		status string
		match  *bool
	}{
		{nil, "not_trusted", codexTestBool(false)},
		{map[string]any{"trusted_hash": hash}, "trusted", codexTestBool(true)},
		{map[string]any{"trusted_hash": "sha256:" + strings.Repeat("0", 64)}, "modified", codexTestBool(false)},
		{map[string]any{"trusted_hash": hash, "enabled": false}, "disabled", codexTestBool(true)},
		{map[string]any{"enabled": false}, "disabled", codexTestBool(false)},
		{map[string]any{"trusted_hash": hash + "/plugin"}, "cannot_verify", nil},
		{map[string]any{"trusted_hash": "sha256:0"}, "cannot_verify", nil},
		{map[string]any{"trusted_hash": 1}, "cannot_verify", nil},
		{map[string]any{"enabled": "false"}, "cannot_verify", nil},
		{map[string]any{"trusted_content_hash": hash}, "cannot_verify", nil},
		{true, "cannot_verify", nil},
	} {
		row := codexHookVerdict("/a", definition, test.state)
		if row.Status != test.status || !reflect.DeepEqual(row.TrustedHashMatches, test.match) {
			t.Fatalf("D08_TRUST_VERDICT: %#v => %#v want %s", test.state, row, test.status)
		}
	}
}

func codexTestBool(value bool) *bool { return &value }

func TestCodexSessionObservation(t *testing.T) {
	for _, test := range []struct{ complete, has, want bool }{
		{false, false, false}, {true, false, true}, {true, true, false}, {false, true, false},
	} {
		if codexZeroSessions(agent.DiagnosticInput{SessionScanComplete: test.complete, HasSessions: test.has}) != test.want {
			t.Fatal("D12_COMPLETE_SCAN_ONLY", test)
		}
	}
}

func TestCodexDiagnosticOfflineAndSafeJSON(t *testing.T) {
	home := codexDiagnosticTestDir(t)
	state := filepath.Join(home, "state")
	root := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", root)
	adapter, in, binary, hooks := newCodexDiagnosticFixture(t, home, state, root)
	marker := filepath.Join(home, "unexpected-launch")
	fake := filepath.Join(home, "bin", "codex")
	writeCodexFixture(t, fake, "#!/bin/sh\necho unexpected >> "+strconv.Quote(marker)+"\nexit 1\n")
	if err := os.Chmod(fake, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))
	in.Found, in.Version, in.SessionScanComplete = true, "0.167.2", true
	result := adapter.diagnoseCodexHooks(in)
	if len(result.Hooks) != 7 || result.Status != "compared" || result.Observation != "Codex: no Codex sessions are registered." {
		t.Fatalf("D09_OFFLINE_RESULT: %#v", result)
	}
	for _, row := range result.Hooks {
		if row.Status != "not_trusted" {
			t.Fatal("D08_EMPTY_VALID_CONFIG_NOT_TRUSTED", row)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("D09_NO_SUBPROCESS", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "config.toml"))
	if err != nil || string(got) != "# no trust yet\n" {
		t.Fatal("D09_NO_TRUST_WRITE", err)
	}
	got, err = os.ReadFile(filepath.Join(root, "hooks.json"))
	if err != nil || string(got) != string(hooks) {
		t.Fatal("D09_NO_HOOK_WRITE", err)
	}
	in.Version = "0.167.3"
	result = adapter.diagnoseCodexHooks(in)
	assertCodexUnverifiedJSON(t, result, binary)
}

func assertCodexUnverifiedJSON(t *testing.T, result codexHooksDiagnostic, binary string) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil || !json.Valid(raw) || result.Status != "cannot_verify" || result.Message != "Codex: cannot verify hook trust (unverified version)." || strings.Contains(string(raw), binary) || strings.Contains(string(raw), "no trust yet") {
		t.Fatal("D11_SAFE_JSON_UNVERIFIED", string(raw), err)
	}
}

func TestCodexDiagnosticRootPrecedence(t *testing.T) {
	home := t.TempDir()
	for _, test := range []struct{ env, want, reason string }{
		{"", filepath.Join(home, ".codex"), ""}, {"   ", filepath.Join(home, ".codex"), ""},
		{filepath.Join(home, "profile"), filepath.Join(home, "profile"), ""},
		{"relative", "", "unsafe_file"}, {"/bad\npath", "", "unsafe_file"}, {"/bad\xff", "", "unsafe_file"},
	} {
		t.Setenv("CODEX_HOME", test.env)
		root, reason := codexDiagnosticRoot(home)
		if root != test.want || reason != test.reason {
			t.Fatal("D07_PROFILE_ROOT", root, reason, test)
		}
	}
}

// newCodexDiagnosticFixture installs the hooks this adapter declares into a fake
// CODEX_HOME and returns the adapter, the doctor's input and the hooks file bytes.
func newCodexDiagnosticFixture(t *testing.T, home, state, root string) (Adapter, agent.DiagnosticInput, string, []byte) {
	t.Helper()
	adapter := New(false, "fixture.json")
	binary := "/fixture/bin/zalki"
	in := agent.DiagnosticInput{Home: home, StateDir: state, ConfigFile: filepath.Join(home, "zalki.json"), Binary: binary, RecordReason: "missing_file"}
	var hooks []byte
	for _, file := range adapter.Configuration(binary, []string{"--config", in.ConfigFile, "--state", state}) {
		if file.Path == ".codex/hooks.json" {
			var err error
			if hooks, err = json.Marshal(file.JSON); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeCodexFixture(t, filepath.Join(root, "hooks.json"), string(hooks))
	writeCodexFixture(t, filepath.Join(root, "config.toml"), "# no trust yet\n")
	return adapter, in, binary, hooks
}

// The doctor puts this notice in the install output and as its own advice; one text.
func TestCodexTrustNoticeIsTheDescriptorNotice(t *testing.T) {
	const want = "Codex: open /hooks and trust the zalki hooks, then send a message in each thread"
	if got := New(false, "fixture.json").Descriptor().InstallNotice; got != want {
		t.Fatalf("InstallNotice = %q", got)
	}
	in := agent.DiagnosticInput{Home: t.TempDir(), RecordReason: "missing_file"}
	if got := New(false, "fixture.json").diagnoseCodexHooks(in).Advice; got != want {
		t.Fatalf("doctor advice = %q", got)
	}
}

// The doctor JSON of one adapter with a trusted, a modified, a disabled and untrusted hooks.
// The golden file is the one recorded from the old CLI code before the engine moved.
func TestCodexMixedTrustStatesGolden(t *testing.T) {
	home := codexDiagnosticTestDir(t)
	state := filepath.Join(home, "state")
	root := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", root)
	adapter, in, _, _ := newCodexDiagnosticFixture(t, home, state, root)
	in.Found, in.Version, in.SessionScanComplete = true, "0.167.2", true
	expected, err := adapter.expectedCodexHookHashes(in)
	if err != nil {
		t.Fatal(err)
	}
	var states strings.Builder
	for _, row := range adapter.diagnoseCodexHooks(in).Hooks {
		switch row.Event {
		case "SessionStart":
			states.WriteString("[hooks.state.\"" + row.Key + "\"]\ntrusted_hash = \"" + expected[row.Event] + "\"\n")
		case "Stop":
			states.WriteString("[hooks.state.\"" + row.Key + "\"]\ntrusted_hash = \"sha256:" + strings.Repeat("0", 64) + "\"\n")
		case "PostToolUse":
			states.WriteString("[hooks.state.\"" + row.Key + "\"]\nenabled = false\n")
		}
	}
	writeCodexFixture(t, filepath.Join(root, "config.toml"), states.String())
	raw, err := json.MarshalIndent(adapter.diagnoseCodexHooks(in), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/doctor_mixed_trust_states.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(raw)+"\n", home, "$HOME"); got != string(want) {
		t.Fatalf("doctor section differs from the golden file:\n%s\nwant\n%s", got, want)
	}
}

// Without an absolute zalki binary the expected hooks cannot be known: nothing is verified.
func TestCodexDiagnoseNeedsTheBinary(t *testing.T) {
	home := codexDiagnosticTestDir(t)
	root := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", root)
	adapter, in, _, _ := newCodexDiagnosticFixture(t, home, filepath.Join(home, "state"), root)
	in.Found, in.Version = true, "0.167.2"
	for _, binary := range []string{"", "relative/zalki"} {
		in.Binary = binary
		if result := adapter.diagnoseCodexHooks(in); result.Status != "cannot_verify" || len(result.Hooks) != 0 {
			t.Fatalf("binary %q: %#v", binary, result)
		}
	}
	if key, _ := adapter.Diagnose(in); key != "codex_hooks" {
		t.Fatalf("section key %q", key)
	}
}
