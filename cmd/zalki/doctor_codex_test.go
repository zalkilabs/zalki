package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"context"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/config"
	"github.com/zalkilabs/zalki/internal/model"
	"syscall"
)

const codexNoticeForTest = "Codex: open /hooks and trust the zalki hooks, then send a message in each thread\n"

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

func newCodexDiagnosticFixture(t *testing.T, home, state, root string) (*commandOptions, []byte) {
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
	return opts, hooks
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

// Doctor may use the existing bounded --version probe, but trust diagnostics
// must not launch app-server or change either native settings file.
func TestDoctorCodexTrustIsOffline(t *testing.T) {
	home := codexDiagnosticTestDir(t)
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
		Codex struct {
			Status string            `json:"status"`
			Hooks  []json.RawMessage `json:"hooks"`
		} `json:"codex_hooks"`
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

func TestAdapterSectionInputs(t *testing.T) {
	report := bus.DiagnosticReport{
		Agents:   []bus.AgentDiagnostic{{Kind: "claude", Found: true, Version: "1.0.0"}, {Kind: "codex", Found: false, Version: "0.1.0"}},
		Sessions: []model.SessionDiagnostic{{Kind: "cursor"}, {Kind: "codex", Retired: true}},
	}
	if found, version := clientVersion(report, "codex"); found || version != "0.1.0" {
		t.Fatal("a client is found by its adapter kind", found, version)
	}
	if found, version := clientVersion(report, "nobody"); found || version != "" {
		t.Fatal("an unknown client is not found")
	}
	for kind, want := range map[string]bool{"codex": true, "cursor": true, "claude": false} {
		if hasSessionOfKind(report, kind) != want {
			t.Fatal("any session record of the kind counts, retired or not:", kind)
		}
	}
}

func TestInstallNoticesComeFromTheAdapters(t *testing.T) {
	var lines []string
	if err := printInstallNotices(adapters.Builtin(), nil, func(line string) error { lines = append(lines, line); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0]+"\n" != codexNoticeForTest {
		t.Fatalf("notices = %q", lines)
	}
}

// stubSectionAdapter reports doctor sections under two keys: one the doctor
// already has and a new one.
type stubSectionAdapter struct{}

func (stubSectionAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "stubsection", Label: "Stub"}
}
func (stubSectionAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn}
}
func (stubSectionAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}
func (stubSectionAdapter) Diagnose(agent.DiagnosticInput) (string, any) { return "agents", "stolen" }

type secondStubSection struct{ stubSectionAdapter }

func (secondStubSection) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "stubsecond", Label: "Second"}
}
func (secondStubSection) Diagnose(agent.DiagnosticInput) (string, any) { return "extra", "added" }

// An adapter's section is added under its own key and never replaces a doctor key.
func TestAdapterSectionNeverReplacesADoctorKey(t *testing.T) {
	registry, err := agent.NewRegistry(stubSectionAdapter{}, secondStubSection{})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	opts := &commandOptions{home: home, registry: registry, snapshot: &config.Snapshot{Paths: config.Paths{
		ConfigFile: config.Value[string]{Value: filepath.Join(home, "zalki.json")}, StateDir: config.Value[string]{Value: filepath.Join(home, "state")},
	}}}
	doctor := map[string]any{"agents": "original"}
	adapterSections(opts, bus.DiagnosticReport{}, doctor)
	if doctor["agents"] != "original" || doctor["extra"] != "added" || len(doctor) != 2 {
		t.Fatalf("doctor sections = %v", doctor)
	}
}

// The doctor reads a file for a diagnostic only when it is a plain, small, UTF-8
// regular file inside a real directory.
func TestReadDiagnosticFile(t *testing.T) {
	dir := codexDiagnosticTestDir(t)
	plain := filepath.Join(dir, "plain.json")
	writeCodexFixture(t, plain, `{"a":1}`)
	if raw, reason := readDiagnosticFile(plain); string(raw) != `{"a":1}` || reason != "" {
		t.Fatalf("a plain file: %q %q", raw, reason)
	}
	if _, reason := readDiagnosticFile(filepath.Join(dir, "none.json")); reason != "missing_file" {
		t.Fatalf("a missing file: %q", reason)
	}
	if _, reason := readDiagnosticFile(filepath.Join(dir, "no-dir", "x.json")); reason != "missing_file" {
		t.Fatalf("a missing directory: %q", reason)
	}
	writeCodexFixture(t, filepath.Join(dir, "bad.json"), "\xff")
	if _, reason := readDiagnosticFile(filepath.Join(dir, "bad.json")); reason != "invalid_data" {
		t.Fatalf("not UTF-8: %q", reason)
	}
	writeCodexFixture(t, filepath.Join(dir, "big.json"), strings.Repeat("x", diagnosticFileLimit+1))
	if _, reason := readDiagnosticFile(filepath.Join(dir, "big.json")); reason != "unsafe_file" {
		t.Fatalf("over the limit: %q", reason)
	}
	if err := os.Symlink(plain, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	if _, reason := readDiagnosticFile(filepath.Join(dir, "link.json")); reason != "unsafe_file" {
		t.Fatalf("a symbolic link: %q", reason)
	}
	if err := os.Symlink(dir, filepath.Join(dir, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if _, reason := readDiagnosticFile(filepath.Join(dir, "linked-dir", "plain.json")); reason != "unsafe_file" {
		t.Fatalf("a symbolic parent: %q", reason)
	}
	if err := os.Mkdir(filepath.Join(dir, "a-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, reason := readDiagnosticFile(filepath.Join(dir, "a-directory")); reason != "unsafe_file" {
		t.Fatalf("a directory: %q", reason)
	}
	// A FIFO must be refused without blocking, even when nobody writes to it.
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, reason := readDiagnosticFile(filepath.Join(dir, "fifo.json")); reason != "unsafe_file" {
		t.Fatalf("a FIFO: %q", reason)
	}
}

// Only the clients that were configured print their line.
func TestInstallNoticesAreOnlyForTheConfiguredClients(t *testing.T) {
	for configured, want := range map[string]int{"codex": 1, "claude": 0, "cursor": 0} {
		var lines []string
		if err := printInstallNotices(adapters.Builtin(), []string{configured}, func(line string) error { lines = append(lines, line); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(lines) != want {
			t.Errorf("configured %s: notices %q, want %d", configured, lines, want)
		}
	}
}
