package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type upgradeFixture struct {
	home, state, prefix, brewLog, childLog, manifest string
}

// newUpgradeFixture builds a disposable Homebrew-like prefix whose "new binary"
// and brew are scripts that record their arguments, over an installed home.
func newUpgradeFixture(t *testing.T, brewScript, childScript string) upgradeFixture {
	t.Helper()
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	root := t.TempDir()
	f := upgradeFixture{home: filepath.Join(root, "home"), state: filepath.Join(root, "home", "state"), prefix: filepath.Join(root, "prefix")}
	f.brewLog, f.childLog = filepath.Join(root, "brew.log"), filepath.Join(root, "child.log")
	f.manifest = filepath.Join(f.state, "installation.json")
	cask := filepath.Join(f.prefix, "Caskroom", "zalki", "2.0.0", "zalki")
	stable := filepath.Join(f.prefix, "bin", "zalki")
	for path, body := range map[string]string{
		cask:                                   "#!/bin/sh\necho \"$@\" >> '" + f.childLog + "'\n" + childScript,
		filepath.Join(f.prefix, "bin", "brew"): "#!/bin/sh\necho \"$@\" >> '" + f.brewLog + "'\n" + brewScript,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(cask, stable); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.home, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, f.home, "", "install", "--state", f.state, "--apply"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // brew must be found via the prefix unless a test adds it.
	f.locate(t, stable, cask)
	return f
}

func (f upgradeFixture) locate(t *testing.T, binary, resolved string) {
	t.Helper()
	old := locateInstallation
	locateInstallation = func() (binaryLocation, error) { return binaryLocation{Binary: binary, Resolved: resolved}, nil }
	t.Cleanup(func() { locateInstallation = old })
}

func (f upgradeFixture) manifestBytes(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f upgradeFixture) logged(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestUpgradeUpgradesBinaryThenHandsOffToNewBuild(t *testing.T) {
	f := newUpgradeFixture(t, "echo noisy brew progress\n", "echo 'Upgrade successful'\n")
	before := f.manifestBytes(t)
	for _, args := range [][]string{{"upgrade", "--state", f.state}, {"upgrade", "--apply", "--state", f.state}} {
		for _, log := range []string{f.brewLog, f.childLog} {
			_ = os.Remove(log)
		}
		out, diagnostic, err := execute(t, f.home, "", args...)
		if err != nil || out != "Upgrade successful\n" || diagnostic != "" {
			t.Fatalf("normal output must be exactly the success line: out=%q stderr=%q err=%v", out, diagnostic, err)
		}
		if got := f.logged(t, f.brewLog); got != "upgrade --cask zalkilabs/tap/zalki" {
			t.Fatal("brew invocation", got)
		}
		if got := f.logged(t, f.childLog); got != "upgrade --finish-upgrade --state="+f.state {
			t.Fatal("new binary handoff", got)
		}
		if f.manifestBytes(t) != before {
			t.Fatal("old build edited configuration instead of the new binary")
		}
	}
}

func TestUpgradeFailuresLeaveConfigurationUntouched(t *testing.T) {
	t.Run("brew fails", func(t *testing.T) {
		f := newUpgradeFixture(t, "echo network unreachable >&2\nexit 7\n", "")
		before := f.manifestBytes(t)
		out, _, err := execute(t, f.home, "", "upgrade", "--state", f.state)
		if err == nil || out != "" || !strings.Contains(err.Error(), "network unreachable") || !strings.Contains(err.Error(), "configuration was not changed") {
			t.Fatal("brew failure not reported", out, err)
		}
		if f.logged(t, f.childLog) != "" || f.manifestBytes(t) != before {
			t.Fatal("failed brew upgrade handed off or changed configuration")
		}
	})
	t.Run("brew missing", func(t *testing.T) {
		f := newUpgradeFixture(t, "", "")
		before := f.manifestBytes(t)
		if err := os.Remove(filepath.Join(f.prefix, "bin", "brew")); err != nil {
			t.Fatal(err)
		}
		out, _, err := execute(t, f.home, "", "upgrade", "--state", f.state)
		if err == nil || out != "" || !strings.Contains(err.Error(), "brew was not found") || !strings.Contains(err.Error(), "--config-only") {
			t.Fatal("missing brew not actionable", out, err)
		}
		if f.logged(t, f.childLog) != "" || f.manifestBytes(t) != before {
			t.Fatal("missing brew changed configuration")
		}
	})
	t.Run("new build cannot refresh configuration", func(t *testing.T) {
		f := newUpgradeFixture(t, "", "echo child failure >&2\nexit 3\n")
		out, diagnostic, err := execute(t, f.home, "", "upgrade", "--state", f.state)
		if err == nil || out != "" || !strings.Contains(err.Error(), "zalki upgrade --config-only") || !strings.Contains(diagnostic, "child failure") {
			t.Fatal("handoff failure not reported", out, diagnostic, err)
		}
	})
}

func TestUpgradeUsesBrewFromPathAndIsIdempotent(t *testing.T) {
	f := newUpgradeFixture(t, "", "echo 'Upgrade successful'\n")
	if err := os.Remove(filepath.Join(f.prefix, "bin", "brew")); err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pathDir, "brew"), []byte("#!/bin/sh\necho \"$@\" >> '"+f.brewLog+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	for range 2 { // already current: brew succeeds without output.
		out, _, err := execute(t, f.home, "", "upgrade", "--state", f.state)
		if err != nil || out != "Upgrade successful\n" {
			t.Fatal("already-current upgrade", out, err)
		}
	}
	if got := f.logged(t, f.brewLog); got != "upgrade --cask zalkilabs/tap/zalki\nupgrade --cask zalkilabs/tap/zalki" {
		t.Fatal("brew from PATH", got)
	}
}

func TestUpgradeDryRunNeverUpgradesOrEdits(t *testing.T) {
	f := newUpgradeFixture(t, "", "")
	before := f.manifestBytes(t)
	out, diagnostic, err := execute(t, f.home, "", "upgrade", "--dry-run", "--state", f.state)
	var report struct {
		DryRun bool   `json:"dry_run"`
		Step   string `json:"binary_upgrade"`
	}
	if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &report) != nil || !report.DryRun || report.Step != "brew upgrade --cask zalkilabs/tap/zalki" {
		t.Fatal("dry-run preview", out, diagnostic, err)
	}
	if f.logged(t, f.brewLog) != "" || f.logged(t, f.childLog) != "" || f.manifestBytes(t) != before {
		t.Fatal("dry-run upgraded or edited configuration")
	}
	if _, _, err := execute(t, f.home, "", "upgrade", "--dry-run", "--apply", "--state", f.state); err == nil {
		t.Fatal("accepted contradictory --dry-run --apply")
	}
}

func TestUpgradeManualInstallIsHonest(t *testing.T) {
	f := newUpgradeFixture(t, "", "")
	manual := filepath.Join(t.TempDir(), "zalki")
	if err := os.WriteFile(manual, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.locate(t, manual, manual)
	before := f.manifestBytes(t)
	out, _, err := execute(t, f.home, "", "upgrade", "--state", f.state)
	if err == nil || out != "" || strings.Contains(out, "Upgrade successful") ||
		!strings.Contains(err.Error(), "not installed with Homebrew") || !strings.Contains(err.Error(), "--config-only") {
		t.Fatal("manual install must fail clearly instead of claiming an upgrade", out, err)
	}
	if f.logged(t, f.brewLog) != "" || f.manifestBytes(t) != before {
		t.Fatal("manual install ran brew or changed configuration after failing")
	}
	out, diagnostic, err := execute(t, f.home, "", "upgrade", "--config-only", "--state", f.state)
	if err != nil || out != "Configuration refreshed\n" || diagnostic != "" {
		t.Fatal("explicit configuration-only refresh", out, diagnostic, err)
	}
	var record struct {
		Binary string `json:"binary"`
	}
	if json.Unmarshal([]byte(f.manifestBytes(t)), &record) != nil || record.Binary != manual {
		t.Fatal("configuration-only refresh did not use the installed binary", record.Binary)
	}
	refreshed := f.manifestBytes(t)
	if out, _, err = execute(t, f.home, "", "upgrade", "--config-only", "--state", f.state); err != nil || out != "Configuration refreshed\n" || f.manifestBytes(t) != refreshed {
		t.Fatal("configuration-only refresh not idempotent", out, err)
	}
	out, _, err = execute(t, f.home, "", "upgrade", "--dry-run", "--state", f.state)
	if err != nil || !strings.Contains(out, "unavailable: not a Homebrew install") {
		t.Fatal("manual dry-run should say the binary step is unavailable", out, err)
	}
}

func TestCaskPrefix(t *testing.T) {
	for path, want := range map[string]string{
		"/opt/homebrew/Caskroom/zalki/0.1.0-rc.6/zalki": "/opt/homebrew",
		"/usr/local/Caskroom/zalki/1.0.0/zalki":         "/usr/local",
	} {
		if got, ok := caskPrefix(path); !ok || got != want {
			t.Fatal("cask path not recognized", path, got)
		}
	}
	for _, path := range []string{"/usr/local/bin/zalki", "/opt/homebrew/Caskroom/other/1/zalki", "/Caskroom/zalki", "/home/me/go/bin/zalki"} {
		if _, ok := caskPrefix(path); ok {
			t.Fatal("non-cask path recognized", path)
		}
	}
}

// Cancellation stops the immediate brew process and bounds waiting for a
// descendant that retains its output pipe, without handing off configuration.
func TestUpgradeCancelStopsBrewBeforeHandoff(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pipe-holder.pid")
	t.Setenv("ZALKI_TEST_BREW_PIPE_PID", pidFile)
	f := newUpgradeFixture(t, `/bin/sh -c 'printf "%s\n" "$$" > "$ZALKI_TEST_BREW_PIPE_PID"; exec /bin/sleep 30' &
wait "$!"
`, "echo 'Upgrade successful'\n")
	before := f.manifestBytes(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := newRootCommand(f.home)
	var output, diagnostic bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs([]string{"upgrade", "--state", f.state})
	result := make(chan error, 1)
	finished := false
	var holder *os.Process
	t.Cleanup(func() {
		cancel()
		if holder != nil {
			if err := holder.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error("kill test pipe holder:", err)
			}
			_ = holder.Release()
		}
		if !finished {
			select {
			case <-result:
			case <-time.After(5 * time.Second):
				t.Error("upgrade did not exit during cleanup")
			}
		}
	})
	go func() { result <- cmd.ExecuteContext(ctx) }()
	// The child publishes its own PID after starting with the output pipe open.
	holder = waitForBrewPipeHolder(t, pidFile)
	cancelled := time.Now()
	cancel()
	select {
	case err := <-result:
		finished = true
		if err == nil || !strings.Contains(err.Error(), "configuration was not changed") {
			t.Fatal("cancelled upgrade did not fail as an upgrade error", err)
		}
		if elapsed := time.Since(cancelled); elapsed > 10*time.Second {
			t.Fatal("cancelled upgrade returned too late", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled upgrade is still waiting for brew")
	}
	if output.String() != "" || f.logged(t, f.childLog) != "" || f.manifestBytes(t) != before {
		t.Fatal("cancelled upgrade handed off or changed configuration", output.String())
	}
}

func waitForBrewPipeHolder(t *testing.T, pidFile string) *os.Process {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(pidFile)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if text := strings.TrimSpace(string(data)); text != "" {
			pid, err := strconv.Atoi(text)
			if err != nil || pid <= 0 {
				t.Fatalf("invalid pipe-holder PID %q: %v", text, err)
			}
			process, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			return process
		}
		if time.Now().After(deadline) {
			t.Fatal("brew pipe holder never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
