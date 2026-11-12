package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/config"
	"github.com/zalkilabs/zalki/internal/model"
)

const codexNoticeForTest = "Codex: open /hooks and trust the zalki hooks, then send a message in each thread\n"

func TestCodexHashExactVersionGate(t *testing.T) {
	for _, version := range []string{"0.167.2", "0.167.0", "0.167.1", "0.167.20", "0.167.2-beta", "0.168.0", "", "codex-cli 0.167.2", "unknown"} {
		for _, found := range []bool{false, true} {
			want := found && version == "0.167.2"
			if got := codexHashVersionSupported([]bus.AgentDiagnostic{{Kind: "codex", Version: version, Found: found}}); got != want {
				t.Fatalf("D01_EXACT_VERSION: %q %t got %t", version, found, got)
			}
		}
	}
	if codexHashVersionSupported(nil) {
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

func TestCodexBoundedFilesAndTOML(t *testing.T) {
	root := t.TempDir()
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
	for _, test := range []struct {
		complete bool
		sessions []model.SessionDiagnostic
		want     bool
	}{
		{false, nil, false}, {true, nil, true},
		{true, []model.SessionDiagnostic{{Kind: "cursor"}}, true},
		{true, []model.SessionDiagnostic{{Kind: "codex", Retired: true}}, false},
		{true, []model.SessionDiagnostic{{Kind: "codex", Liveness: "exited"}}, false},
	} {
		if codexZeroSessions(bus.DiagnosticReport{SessionScanComplete: test.complete, Sessions: test.sessions}) != test.want {
			t.Fatal("D12_COMPLETE_SCAN_ONLY", test)
		}
	}
}

func TestCodexDiagnosticOfflineAndSafeJSON(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	root := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", root)
	opts, binary, hooks := newCodexDiagnosticFixture(t, home, state, root)
	marker := filepath.Join(home, "unexpected-launch")
	fake := filepath.Join(home, "bin", "codex")
	writeCodexFixture(t, fake, "#!/bin/sh\necho unexpected >> "+strconv.Quote(marker)+"\nexit 1\n")
	if err := os.Chmod(fake, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))
	report := bus.DiagnosticReport{SessionScanComplete: true, Agents: []bus.AgentDiagnostic{{Kind: "codex", Found: true, Version: "0.167.2", Binary: fake}}}
	result := diagnoseCodexHooks(opts, report)
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
	report.Agents[0].Version = "0.167.3"
	result = diagnoseCodexHooks(opts, report)
	assertCodexUnverifiedJSON(t, result, binary)
}

func assertCodexUnverifiedJSON(t *testing.T, result codexHooksDiagnostic, binary string) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil || !json.Valid(raw) || result.Status != "cannot_verify" || result.Message != "Codex: cannot verify hook trust (unverified version)." || strings.Contains(string(raw), binary) || strings.Contains(string(raw), "no trust yet") {
		t.Fatal("D11_SAFE_JSON_UNVERIFIED", string(raw), err)
	}
}

func TestCodexFailedApplyHasNoTrustNotice(t *testing.T) {
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	writeCodexFixture(t, filepath.Join(home, ".codex", "hooks.json"), "{broken")
	out, _, err := execute(t, home, "", "install", "--apply", "--state", filepath.Join(home, "state"))
	if err == nil || strings.Contains(out, codexNoticeForTest) {
		t.Fatal("D10_NO_NOTICE_FAILED_APPLY", out, err)
	}
}

func TestCodexUpgradeRefreshKeepsSuccessOutput(t *testing.T) {
	f := newUpgradeFixture(t, "", "")
	binary, err := installationBinary()
	if err != nil {
		t.Fatal(err)
	}
	f.locate(t, binary, binary)
	for _, mode := range []struct{ flag, want string }{{"--config-only", configOnlyDone}, {"--finish-upgrade", upgradeSuccess}} {
		out, _, err := execute(t, f.home, "", "upgrade", "--state", f.state, mode.flag)
		if err != nil || out != mode.want+"\n" {
			t.Fatal("D10_UPGRADE_SUCCESS_OUTPUT_ONLY", out, err)
		}
	}
}

func newCodexDiagnosticFixture(t *testing.T, home, state, root string) (*commandOptions, string, []byte) {
	t.Helper()
	opts := &commandOptions{home: home, registry: adapters.Builtin(), snapshot: &config.Snapshot{Paths: config.Paths{
		ConfigFile: config.Value[string]{Value: filepath.Join(home, "zalki.json")}, StateDir: config.Value[string]{Value: state},
	}}}
	binary, err := installationBinary()
	if err != nil {
		t.Fatal(err)
	}
	files, err := installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value)(binary, state)
	if err != nil {
		t.Fatal(err)
	}
	var hooks []byte
	for _, file := range files {
		if file.Path == ".codex/hooks.json" {
			hooks, err = json.Marshal(file.JSON)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	writeCodexFixture(t, filepath.Join(root, "hooks.json"), string(hooks))
	writeCodexFixture(t, filepath.Join(root, "config.toml"), "# no trust yet\n")
	return opts, binary, hooks
}

func TestCodexInstallNoticeSuccessOnly(t *testing.T) {
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	state := filepath.Join(home, "state")
	for range 2 {
		out, _, err := execute(t, home, "", "install", "--apply", "--state", state)
		if err != nil || strings.Count(out, codexNoticeForTest) != 1 {
			t.Fatal("D10_NOTICE_SUCCESS_IDEMPOTENT", out, err)
		}
	}
	for _, args := range [][]string{{"install", "--out", filepath.Join(home, "drafts")}, {"uninstall", "--apply"}} {
		out, _, err := execute(t, home, "", append(args, "--state", state)...)
		if err != nil || strings.Contains(out, codexNoticeForTest) {
			t.Fatal("D10_NO_NOTICE_DRAFT_UNINSTALL", out, err)
		}
	}
	t.Setenv("CODEX_HOME", "relative")
	out, _, err := execute(t, home, "", "install", "--apply", "--state", state)
	if err == nil || strings.Contains(out, codexNoticeForTest) {
		t.Fatal("D10_NO_NOTICE_FAILURE", out, err)
	}
}

func TestCodexUpgradeHandoffKeepsSuccessOutput(t *testing.T) {
	f := newUpgradeFixture(t, "", "echo 'Upgrade successful'\n")
	out, _, err := execute(t, f.home, "", "upgrade", "--state", f.state)
	if err != nil || out != upgradeSuccess+"\n" {
		t.Fatal("D10_UPGRADE_HANDOFF_SUCCESS_OUTPUT_ONLY", out, err)
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

// Doctor may use the existing bounded --version probe, but trust diagnostics
// must not launch app-server or change either native settings file.
func TestDoctorCodexTrustIsOffline(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	if _, _, err := execute(t, home, "", "install", "--apply", "--state", state); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".codex")
	hooksBefore, err := os.ReadFile(filepath.Join(root, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(filepath.Join(root, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(home, "codex-calls")
	fake := filepath.Join(home, "bin", "codex")
	writeCodexFixture(t, fake, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+strconv.Quote(log)+"\n[ \"$#\" = 1 ] && [ \"$1\" = --version ] || exit 1\nprintf 'codex-cli 0.167.2\\n'\n")
	if err := os.Chmod(fake, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake))
	out, _, err := execute(t, home, "", "doctor", "--state", state)
	var report struct {
		Codex codexHooksDiagnostic `json:"codex_hooks"`
	}
	if err != nil || json.Unmarshal([]byte(out), &report) != nil || report.Codex.Status != "compared" || len(report.Codex.Hooks) != 7 {
		t.Fatal("D11_DOCTOR_JSON", out, err)
	}
	calls, err := os.ReadFile(log)
	if err != nil || string(calls) != "--version\n" {
		t.Fatal("D09_ONLY_EXISTING_VERSION_PROBE", string(calls), err)
	}
	for path, before := range map[string][]byte{"hooks.json": hooksBefore, "config.toml": configBefore} {
		after, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(after) != string(before) {
			t.Fatal("D09_SETTINGS_UNCHANGED", path, err)
		}
	}
}
