package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

func execute(t *testing.T, home, input string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCommand(home)
	var output, diagnostic bytes.Buffer
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return output.String(), diagnostic.String(), err
}

func TestHelpAndCompletionDoNotTouchState(t *testing.T) {
	for _, args := range [][]string{
		nil, {"--help"}, {"help", "prune"}, {"mcp", "--help"},
		{"hook", "--help"}, {"cursor-listen", "--help"},
		{"install", "--help"}, {"upgrade", "--help"}, {"uninstall", "--help"}, {"completion", "bash"},
		{"completion", "zsh"}, {"completion", "fish"}, {"completion", "powershell"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := t.TempDir()
			out, diagnostic, err := execute(t, home, "", args...)
			if err != nil || out == "" || diagnostic != "" {
				t.Fatalf("help/completion failed: %v, stderr=%q", err, diagnostic)
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatal("help/completion touched state", err)
			}
		})
	}
}

func TestGlobalFlagPositionsAndJSONOutput(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "registry")
	for _, args := range [][]string{
		{"--state", state, "doctor"},
		{"doctor", "--state", state},
	} {
		out, diagnostic, err := execute(t, home, "", args...)
		if err != nil || diagnostic != "" {
			t.Fatalf("doctor failed: %v, stderr=%q", err, diagnostic)
		}
		var report struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(out), &report); err != nil || report.State != state {
			t.Fatalf("global flags or JSON changed: %+v %v", report, err)
		}
	}
}

func TestRejectedArgumentsNeverWriteProtocolOutputOrState(t *testing.T) {
	for _, args := range [][]string{
		{"unknown-command"}, {"--unknown-flag"}, {"list", "--unknown-flag"},
		{"doctor", "--experimental-claude=false"},
		{"hook"}, {"hook", "--agent", "unknown"},
		{"setup", "--apply"},
		{"prune", "--dry-run=maybe"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := t.TempDir()
			out, diagnostic, err := execute(t, home, "{}", args...)
			if err == nil || out != "" || diagnostic != "" {
				t.Fatalf("bad invocation was not returned as a silent error: %v, stdout=%q, stderr=%q", err, out, diagnostic)
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatal("bad invocation touched state", err)
			}
		})
	}
	for _, name := range []string{"version", "mcp", "hook", "cursor-listen", "send", "list", "doctor", "prune", "setup", "install", "upgrade", "uninstall"} {
		t.Run(name+" extra argument", func(t *testing.T) {
			out, diagnostic, err := execute(t, t.TempDir(), "{}", name, "unexpected")
			if err == nil || out != "" || diagnostic != "" {
				t.Fatalf("positional argument accepted: %v, stdout=%q, stderr=%q", err, out, diagnostic)
			}
		})
	}
}

func TestVersionAndFreshCommandFlags(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{{"--version"}, {"version"}} {
		out, diagnostic, err := execute(t, home, "", args...)
		if err != nil || diagnostic != "" || out != bus.BuildVersion()+"\n" {
			t.Fatalf("version changed: %v stdout=%q stderr=%q", err, out, diagnostic)
		}
	}
	state := filepath.Join(home, "custom")
	if _, _, err := execute(t, home, "", "list", "--state", state); err != nil {
		t.Fatal(err)
	}
	out, _, err := execute(t, home, "", "doctor")
	var report struct{ State string }
	if err != nil || json.Unmarshal([]byte(out), &report) != nil || report.State != filepath.Join(home, ".local", "state", "zalki") {
		t.Fatal("flags leaked between command trees", err)
	}
}

func TestRepeatablePruneSelectionAndDryRun(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, "state")
	store := bus.Store{Dir: state}
	var ids []string
	for _, id := range []string{"00000000-0000-4000-8000-000000000021", "00000000-0000-4000-8000-000000000022"} {
		session, _, err := store.Register("cursor", id, "/fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, session.ID)
	}
	args := []string{"prune", "--state", state, "--session", ids[0], "--session", ids[1]}
	for _, dry := range []bool{true, false} {
		argv := append([]string{}, args...)
		if dry {
			argv = append(argv, "--dry-run")
		}
		out, diagnostic, err := execute(t, home, "", argv...)
		var report bus.PruneReport
		if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &report) != nil || report.DryRun != dry || len(report.Sessions) != 2 {
			t.Fatalf("prune flags changed semantics: %+v %v stderr=%q", report, err, diagnostic)
		}
		listed, err := store.List()
		if err != nil || (dry && len(listed) != 2) || (!dry && len(listed) != 0) {
			t.Fatal("prune did not respect dry run", err)
		}
	}
}

func TestGeneratedSetupRetainsInstalledCommandSyntax(t *testing.T) {
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	state := filepath.Join(home, "state")
	for _, name := range []string{"setup", "install"} {
		outDir := filepath.Join(home, name)
		_, diagnostic, err := execute(t, home, "", name, "--state", state, "--out", outDir)
		if err != nil || diagnostic != "" {
			t.Fatal("setup failed", err, diagnostic)
		}
		config, err := os.ReadFile(filepath.Join(outDir, "cursor-mcp.json"))
		if err != nil {
			t.Fatal(err)
		}
		var value struct {
			Servers map[string]struct{ Args []string } `json:"mcpServers"`
		}
		if json.Unmarshal(config, &value) != nil || strings.Join(value.Servers["zalki"].Args, "|") != "--state|"+state+"|mcp" {
			t.Fatal("installed MCP invocation changed")
		}
	}
}

func TestPreviewFlagIsAcceptedWithoutPropagatingIt(t *testing.T) {
	home := t.TempDir()
	outDir := filepath.Join(home, "drafts")
	_, _, err := execute(t, home, "", "--experimental-claude", "setup", "--out", outDir)
	if err != nil {
		t.Fatal("installed preview invocation stopped working", err)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(outDir, entry.Name()))
		if err != nil || bytes.Contains(data, []byte("--experimental-claude")) {
			t.Fatal("new configuration retains the obsolete flag", entry.Name(), err)
		}
	}
	help, _, err := execute(t, home, "", "--help")
	if err != nil || strings.Contains(help, "--experimental-claude") {
		t.Fatal("obsolete flag remains in help", err)
	}
}

func TestSendCredentialsRemainOutsideMessageJSON(t *testing.T) {
	home := t.TempDir()
	input := `{"capability":"fixture-private-capability"}`
	out, diagnostic, err := execute(t, home, input, "send")
	if err == nil || !strings.Contains(err.Error(), "ZALKI_CAPABILITY") || strings.Contains(err.Error(), "fixture-private-capability") || out != "" || diagnostic != "" {
		t.Fatal("send credential boundary changed")
	}
}

func TestLifecycleCommandPreviewsAndApplies(t *testing.T) {
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if _, _, err := execute(t, home, "", "install", "--state", state, "--apply"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"upgrade", "uninstall"} {
		args := []string{action, "--state", state}
		out, diagnostic, err := execute(t, home, "", args...)
		var report install.Report
		if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &report) != nil || !report.DryRun || report.Action != action {
			t.Fatal("lifecycle preview failed", action, err)
		}
		if _, err := os.Stat(filepath.Join(state, "installation.json")); err != nil {
			t.Fatal("preview removed record", err)
		}
		out, diagnostic, err = execute(t, home, "", append(args, "--apply")...)
		if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &report) != nil || report.DryRun {
			t.Fatal("lifecycle apply failed", action, err)
		}
	}
}
