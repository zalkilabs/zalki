package adapters_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters/antigravity"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/install"
)

// These tests install the Antigravity files with the real installer into a
// temporary home, next to the entries of another tool, and read them with the
// adapter's doctor section. Nothing touches a real ~/.gemini.

const antigravityBinary = "/opt/zalki tools/zalki"

// antigravityConfigFile is the Zalki settings file of a temporary home. The
// installer gives it to the hook command and the doctor to the adapter, as the CLI does.
func antigravityConfigFile(home string) string { return filepath.Join(home, "config.json") }

// antigravityDeclare converts the adapter's declarations the way cmd/zalki does.
func antigravityDeclare(home string) install.Declare {
	return func(binary, state string) ([]install.ConfigFile, error) {
		declarations := antigravity.New().Configuration(binary, []string{"--config", antigravityConfigFile(home), "--state", state})
		files := make([]install.ConfigFile, 0, len(declarations))
		for _, declaration := range declarations {
			files = append(files, install.ConfigFile{Path: declaration.Path, Draft: declaration.Draft, JSON: declaration.JSON, TOML: declaration.TOML, ConfigRootEnv: declaration.ConfigRootEnv, OwnedEntry: declaration.OwnedEntry})
		}
		return files, nil
	}
}

const antigravityForeignHooks = `{
  "other-tool": {"enabled": false, "Stop": [{"type": "command", "command": "echo \"hi\" && true", "timeout": 30.0}], "numbers": [1.50, 1e3, 12345678901234567890]},
  "zeta": {}
}`

func seedAntigravityHooks(t *testing.T, home, content string) {
	t.Helper()
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func antigravityRequest(home, action string) install.Request {
	return install.Request{Action: action, Binary: antigravityBinary, StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true}
}

func antigravityDiagnose(t *testing.T, home string) map[string]any {
	t.Helper()
	state := filepath.Join(home, "state")
	raw, err := os.ReadFile(filepath.Join(state, "installation.json"))
	reason := ""
	if err != nil {
		raw, reason = nil, "missing_file"
	}
	targets, recordReason := install.RecordedTargets(raw, reason, home, state)
	_, section := antigravity.New().Diagnose(agent.DiagnosticInput{
		Home: home, StateDir: state, ConfigFile: antigravityConfigFile(home), Binary: antigravityBinary, Found: true, Version: "1.5.18",
		RecordReason: recordReason, RecordedTargets: targets,
	})
	encoded, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestAntigravityInstallWritesBothFilesAndTheDoctorAcceptsThem(t *testing.T) {
	isolatedClientRoots(t)
	home := t.TempDir()
	seedAntigravityHooks(t, home, antigravityForeignHooks)
	if _, err := install.Manage(antigravityDeclare(home), antigravityRequest(home, "install")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".gemini/config/hooks.json", ".gemini/config/mcp_config.json"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(name))); err != nil {
			t.Fatalf("install did not write %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, ".gemini", "config", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil || top["zalki"] == nil || top["other-tool"] == nil || top["zeta"] == nil {
		t.Fatalf("hooks file after install = %s (%v)", raw, err)
	}
	if section := antigravityDiagnose(t, home); section["status"] != "ok" {
		t.Fatalf("doctor section after install = %v", section)
	}
}

// Disabling the entry is the user's right: upgrade keeps the key, and the doctor says so.
func TestAntigravityDoctorSeesADisabledEntryAfterAnUpgradeKeptIt(t *testing.T) {
	isolatedClientRoots(t)
	home := t.TempDir()
	if _, err := install.Manage(antigravityDeclare(home), antigravityRequest(home, "install")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	top["zalki"].(map[string]any)["enabled"] = false
	edited, _ := json.Marshal(top)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	section := antigravityDiagnose(t, home)
	problems, _ := section["problems"].([]any)
	if section["status"] != "attention" || len(problems) != 1 || problems[0].(map[string]any)["code"] != "entry_disabled" {
		t.Fatalf("doctor section = %v", section)
	}
	request := antigravityRequest(home, "upgrade")
	request.Binary = "/opt/new tools/zalki"
	if _, err := install.Manage(antigravityDeclare(home), request); err != nil {
		t.Fatalf("upgrade over a disabled entry: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(mustDecodeAntigravity(t, after)["zalki"].(map[string]any)["enabled"], false) {
		t.Fatalf("upgrade did not keep the user's key: %s (%v)", after, err)
	}
}

func mustDecodeAntigravity(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	return top
}

func TestAntigravityUninstallLeavesOtherToolsAndTheDoctorSaysNotInstalled(t *testing.T) {
	isolatedClientRoots(t)
	home := t.TempDir()
	seedAntigravityHooks(t, home, antigravityForeignHooks)
	if _, err := install.Manage(antigravityDeclare(home), antigravityRequest(home, "install")); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Manage(antigravityDeclare(home), antigravityRequest(home, "uninstall")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".gemini", "config", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	top := mustDecodeAntigravity(t, raw)
	if _, found := top["zalki"]; found || top["other-tool"] == nil || top["zeta"] == nil {
		t.Fatalf("hooks file after uninstall = %s", raw)
	}
	if section := antigravityDiagnose(t, home); section["status"] != "not_installed" {
		t.Fatalf("doctor section after uninstall = %v", section)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config", "mcp_config.json")); err != nil {
		t.Logf("the MCP file is %v after uninstall", err)
	}
}

// The declaration is accepted by the registry and by the installer without any change to core.
func TestAntigravityDeclarationFitsTheRegistryAndTheInstallerTypes(t *testing.T) {
	registry, err := agent.NewRegistry(antigravity.New())
	if err != nil {
		t.Fatal(err)
	}
	declarations, err := registry.Configuration(antigravityBinary, []string{"--state", "/fixture/state"})
	if err != nil || len(declarations) != 2 {
		t.Fatalf("declarations = %+v, %v", declarations, err)
	}
	if declarations[0].Path >= declarations[1].Path {
		t.Fatalf("declarations are not sorted by path: %v", declarations)
	}
}
