package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runInstalledBinary runs the real zalki binary with a temporary home and nothing
// else from the environment. It returns stdout, stderr and the exit code.
func runInstalledBinary(t *testing.T, binary, home string, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = []string{"HOME=" + home, "PATH=" + t.TempDir(), "LC_ALL=C"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// All four clients are there, but the settings file of Antigravity is not valid
// JSON. The upgrade configures the other three, skips Antigravity with the file and
// the next step, and exits 1. After the file is fixed, the same command configures
// Antigravity.
func TestUpgradeSkipsAnAntigravityFileThatIsNotValidThenConfiguresIt(t *testing.T) {
	binary := checkRealBinary(t)
	home := seedHomeWithAllClients(t)
	state := filepath.Join(home, "state")
	mcp := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	const broken = `{"mcpServers": {"a": 1 "b": 2}}`
	if err := os.WriteFile(mcp, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	out, diagnostic, code := runInstalledBinary(t, binary, home, "upgrade", "--config-only", "--state", state)
	if code != 1 {
		t.Fatalf("exit code %d, want 1\n%s%s", code, out, diagnostic)
	}
	for _, want := range []string{
		"Skipped Antigravity: " + mcp + ": invalid character '\"' after object key:value pair.\n",
		"Next: fix that file, then run zalki upgrade.\n",
	} {
		if !strings.Contains(diagnostic, want) {
			t.Errorf("the messages lack %q:\n%s", want, diagnostic)
		}
	}
	for _, name := range []string{".claude.json", filepath.Join(".codex", "hooks.json"), filepath.Join(".cursor", "mcp.json")} {
		if !strings.Contains(readFile(t, filepath.Join(home, name)), "zalki") {
			t.Errorf("%s was not configured", name)
		}
	}
	if got := readFile(t, mcp); got != broken {
		t.Fatalf("the unusable file was changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("the hooks file of the skipped client was written (%v)", err)
	}

	if err := os.WriteFile(mcp, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, diagnostic, code = runInstalledBinary(t, binary, home, "upgrade", "--config-only", "--state", state)
	if code != 0 {
		t.Fatalf("exit code %d after the fix, want 0\n%s%s", code, out, diagnostic)
	}
	for _, name := range []string{"mcp_config.json", "hooks.json"} {
		if !strings.Contains(readFile(t, filepath.Join(home, ".gemini", "config", name)), "zalki") {
			t.Errorf("%s of Antigravity was not configured after the fix", name)
		}
	}
}

// seedHomeWithAllClients makes a temporary home where Claude, Codex, Cursor and
// Antigravity are present.
func seedHomeWithAllClients(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	seedClientDirs(t, home)
	return home
}

// A home with a bare ~/.gemini, as another tool may leave it, is not a home with
// Antigravity: install and upgrade write no Antigravity file and name it as not found.
func TestABareGeminiDirectoryIsNotAntigravity(t *testing.T) {
	home := presenceHome(t, ".claude", ".gemini")
	state := filepath.Join(home, "state")
	for _, args := range [][]string{
		{"install", "--state", state, "--yes"},
		{"upgrade", "--config-only", "--state", state},
	} {
		out, diagnostic, err := executeBare(t, home, "", args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s%s", args, err, out, diagnostic)
		}
		if got := out + diagnostic; !strings.Contains(got, "Not found: Antigravity, Codex, Cursor.") || strings.Contains(got, "Antigravity: restart agy") {
			t.Fatalf("%v: antigravity is reported as present or not named as not found:\n%s", args, got)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config")); !os.IsNotExist(err) {
		t.Fatalf("a directory was made in the bare .gemini directory (%v)", err)
	}
}
