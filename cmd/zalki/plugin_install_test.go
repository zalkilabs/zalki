package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

const nativeInstallManifest = `{"name":"demo","language":"native","purpose":"test notices","build":null,"artifact":"bin/plugin","requests":{"feed":true,"read":true,"send":false,"inbox_read":true,"accept_from":false}}`

func writeInstallFolder(t *testing.T, manifest string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plugin-src")
	files["zalki-plugin.json"] = manifest
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if strings.HasPrefix(body, "#!") {
			mode = 0700
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func nativeInstallFolder(t *testing.T) string {
	t.Helper()
	return writeInstallFolder(t, nativeInstallManifest, map[string]string{"bin/plugin": "#!/bin/sh\nexit 0\n"})
}

// pluginInstallLocal runs plugins install with fake keys and a chosen terminal mode.
func pluginInstallLocal(t *testing.T, state string, mode installChoice, input string, args ...string) (string, error) {
	t.Helper()
	previous := installTerminal
	installTerminal = func(io.Reader) (installChoice, func()) { return mode, func() {} }
	t.Cleanup(func() { installTerminal = previous })
	cmd := newRootCommand(t.TempDir())
	output := &cappedOutput{limit: 4 << 20}
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(output)
	cmd.SetErr(output)
	cmd.SetArgs(append([]string{"--state", state, "--log-level", "off", "plugins", "install"}, args...))
	err := cmd.ExecuteContext(t.Context())
	return output.String(), err
}

// cappedOutput fails writes past its limit, so a screen loop that never ends
// fails the test quickly instead of growing memory without bound.
type cappedOutput struct {
	bytes.Buffer
	limit   int
	watch   string // when this text is written, onWatch runs once
	onWatch func()
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.limit {
		return 0, errors.New("test output limit reached")
	}
	n, err := c.Buffer.Write(p)
	if c.onWatch != nil && strings.Contains(c.String(), c.watch) {
		c.onWatch()
		c.onWatch = nil
	}
	return n, err
}

// WriteString must be capped too: io.WriteString would use the embedded Buffer's.
func (c *cappedOutput) WriteString(s string) (int, error) {
	return c.Write([]byte(s))
}

type installStatus struct {
	pluginfeed.PluginFeedStatus
	Install    *pluginfeed.PluginInstallReceipt `json:"install"`
	PinMatches *bool                            `json:"pin_matches"`
}

func readInstallStatus(t *testing.T, state, name string) installStatus {
	t.Helper()
	raw, err := pluginOwnerLocal(t, state, "status", name)
	var status installStatus
	if err != nil || json.Unmarshal(raw, &status) != nil {
		t.Fatal("status failed", string(raw), err)
	}
	return status
}

func assertNothingInstalled(t *testing.T, state string) {
	t.Helper()
	const name = "demo"
	for _, file := range pluginStateFiles(t, state) {
		if strings.HasPrefix(file, name+".json") || strings.HasPrefix(file, name+".bin") || strings.HasPrefix(file, name+".data") || strings.HasPrefix(file, "id-") || strings.HasPrefix(file, ".staging/"+name) {
			t.Fatal("failed install left", file, pluginStateFiles(t, state))
		}
	}
}

func TestPluginInstallGoBuildEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	store, session := pluginOwnerFixture(t)
	manifest := `{"name":"gonote","language":"go","purpose":"desktop notices","build":["go","build","-mod=readonly","-o","plugin","."],"artifact":"plugin","args":["--quiet"],"requests":{"feed":true,"read":false,"send":false,"inbox_read":false,"accept_from":false}}`
	folder := writeInstallFolder(t, manifest, map[string]string{
		"go.mod":  "module example.com/gonote\n\ngo 1.22\n",
		"main.go": "package main\n\nimport (\"io\"; \"os\")\n\nfunc main() { _, _ = io.Copy(io.Discard, os.Stdin) }\n",
	})
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, " y", folder)
	if err != nil {
		t.Fatal(output, err)
	}
	for _, want := range []string{"Install gonote", "Build: go build -mod=readonly -o plugin .\r\nTool: /", "Access per session: changes = see state and message status, no text\r\n", "Publisher proof: none", "no sandbox", "Building...", "Installed and enabled gonote. Access: changes from 1 session(s). Not running yet"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	tree := assertGoInstallStatus(t, store.Dir, session.ID, output)
	assertInstalledLifecycle(t, store.Dir, session.ID, tree)
}

func assertGoInstallStatus(t *testing.T, state, session, output string) string {
	t.Helper()
	status := readInstallStatus(t, state, "gonote")
	bin := filepath.Join(state, "plugins", "gonote.bin", status.Launch.SHA256)
	if !status.Enabled || !slices.Equal(status.Sessions, []string{session}) || status.Capabilities != nil || status.PinMatches == nil || !*status.PinMatches || status.Launch.Executable != bin || !slices.Equal(status.Launch.Args, []string{"--quiet"}) {
		t.Fatalf("installed status wrong: %+v", status)
	}
	r := status.Install
	assertGoReceipt(t, r, status.Launch.SHA256)
	if !strings.Contains(output, "Snapshot: "+r.TreeSHA256) {
		t.Fatal("screen hash differs from the receipt")
	}
	if info, err := os.Stat(bin); err != nil || info.Mode().Perm() != 0500 {
		t.Fatal("pinned artifact mode", info, err)
	}
	files := strings.Join(pluginStateFiles(t, state), " ")
	if files != ".staging/ gonote.bin/ gonote.bin/"+status.Launch.SHA256+" gonote.data/ gonote.json gonote.lock id-UUID.json" {
		t.Fatal("unexpected state after install:", files)
	}
	return r.TreeSHA256
}

func assertGoReceipt(t *testing.T, r *pluginfeed.PluginInstallReceipt, artifact string) {
	t.Helper()
	if r == nil || !strings.HasSuffix(r.Source, "plugin-src") || r.ArtifactSHA256 != artifact || !filepath.IsAbs(r.Tool) || len(r.ToolSHA256) != 64 || !slices.Equal(r.Build, []string{"go", "build", "-mod=readonly", "-o", "plugin", "."}) {
		t.Fatalf("receipt wrong: %+v", r)
	}
}

// Disable, configure and enable keep the receipt; remove deletes the artifact.
func assertInstalledLifecycle(t *testing.T, state, session, tree string) {
	t.Helper()
	for _, args := range [][]string{{"disable", "gonote"}, {"configure", "gonote", "--session", session}, {"enable", "gonote"}} {
		if raw, err := pluginOwnerLocal(t, state, args...); err != nil {
			t.Fatal(args, string(raw), err)
		}
	}
	if again := readInstallStatus(t, state, "gonote"); again.Install == nil || again.Install.TreeSHA256 != tree {
		t.Fatal("lifecycle dropped the install receipt", again.Install)
	}
	for _, args := range [][]string{{"disable", "gonote"}, {"remove", "gonote"}} {
		if raw, err := pluginOwnerLocal(t, state, args...); err != nil {
			t.Fatal(args, string(raw), err)
		}
	}
	if files := strings.Join(pluginStateFiles(t, state), " "); files != ".staging/ gonote.lock" {
		t.Fatal("remove left install files:", files)
	}
}

func TestPluginInstallNativeAndCancelPaths(t *testing.T) {
	store, session := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	assertInstallCancels(t, store.Dir, folder)
	// Right+Space: read; Down+Space: own inbox; Left, Up, Space: changes; then y.
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "\x1b[C \x1b[B \x1b[D\x1b[A y", folder)
	if err != nil {
		t.Fatal(output, err)
	}
	assertNativeChoices(t, store.Dir, session.ID, output)
	// A second install of the same name stops before the screen.
	if output, err := pluginInstallLocal(t, store.Dir, installRawKeys, " y", folder); err == nil || !strings.Contains(err.Error(), "already installed") || strings.Contains(output, "Install demo") {
		t.Fatal("reinstall was not refused early", output, err)
	}
}

func assertNativeChoices(t *testing.T, state, session, output string) {
	t.Helper()
	status := readInstallStatus(t, state, "demo")
	c := status.Capabilities
	if !status.Enabled || !slices.Equal(status.Sessions, []string{session}) || c == nil || !slices.Equal(c.Read, []string{session}) || !c.InboxRead || c.PinSHA256 != status.Launch.PinSHA256 || c.Installation != status.ID {
		t.Fatalf("choices not recorded exactly: %+v %+v", status, c)
	}
	if status.Install.Tool != "" || len(status.Install.Build) != 0 || status.Install.Build == nil {
		t.Fatal("native receipt claimed a build", status.Install)
	}
	if !strings.Contains(output, "Build: none (ready-made executable)") {
		t.Fatal(output)
	}
}

func assertInstallCancels(t *testing.T, state, folder string) {
	t.Helper()
	for _, input := range []string{"n", "\r y", "q", "\x03 y", "", "y n", "  y n", "\x1b[B y n"} {
		output, err := pluginInstallLocal(t, state, installRawKeys, input, folder)
		if err != nil || !strings.Contains(output, "Cancelled. Nothing was installed.") {
			t.Fatalf("input %q: %s %v", input, output, err)
		}
		if strings.HasSuffix(input, "y n") && !strings.Contains(output, "Select at least one session") {
			t.Fatal("empty yes was not explained", output)
		}
		assertNothingInstalled(t, state)
	}
}

func TestPluginInstallPlainLineFallback(t *testing.T) {
	store, session := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	output, err := pluginInstallLocal(t, store.Dir, installPlainLines, "9\n1\n\nno\ny\n", folder)
	if err != nil {
		t.Fatal(output, err)
	}
	for _, want := range []string{"1. ", "enter session numbers, e.g. 1,3", "Use a number from the list (1).", "Installed and enabled demo"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	if status := readInstallStatus(t, store.Dir, "demo"); !slices.Equal(status.Sessions, []string{session.ID}) || status.Capabilities != nil {
		t.Fatal("fallback recorded wrong choices", status)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "disable", "demo"); err != nil {
		t.Fatal(string(raw), err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "remove", "demo"); err != nil {
		t.Fatal(string(raw), err)
	}
	for input, want := range map[string]string{
		"1\nn\nn\n":  "Cancelled. Nothing was installed.",
		"\n\nn\ny\n": "Select at least one session for changes. Nothing was installed.",
		"":           "Cancelled. Nothing was installed.",
		"x\nx\nx\n":  "no valid session numbers for changes; nothing was installed",
		"1\n\nn\ny":  "Cancelled. Nothing was installed.", // closed input after a final y is No
	} {
		output, err := pluginInstallLocal(t, store.Dir, installPlainLines, input, folder)
		if err != nil {
			output += err.Error()
		}
		if strings.Contains(output, "Installed and enabled") || !strings.Contains(output, want) {
			t.Fatalf("input %q: want %q, got: %s", input, want, output)
		}
		assertNothingInstalled(t, store.Dir)
	}
}

func TestPluginInstallReadsArrowKeys(t *testing.T) {
	for input, want := range map[string][]string{
		"\x1b[A\x1b[B\x1b[C\x1b[D\x1bOC\x1bx\r\x03y": {"up", "down", "right", "left", "right", "esc", "x", "enter", "ctrl-c", "y"},
		// A key after a lone ESC is kept, not swallowed as part of a sequence.
		"\x1by\x1bn\x1bq\x1b\x03\x1b\r": {"esc", "y", "esc", "n", "esc", "q", "esc", "ctrl-c", "esc", "enter"},
		"\x1b\x1b[A\x1b":                {"esc", "up", "esc"},
	} {
		keys := bufio.NewReader(strings.NewReader(input))
		var got []string
		for k := readInstallKey(keys); k != "eof"; k = readInstallKey(keys) {
			got = append(got, k)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("input %q: keys %q, want %q", input, got, want)
		}
	}
}

func TestPluginInstallRawEscKeepsNextKey(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "\x1b \x1by", nativeInstallFolder(t))
	if err != nil || !strings.Contains(output, "Installed and enabled demo") {
		t.Fatalf("keys after ESC were lost: %v\n%s", err, output)
	}
}

// panicWriter panics when the raw screen is drawn.
type panicWriter struct{ bytes.Buffer }

func (w *panicWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("\x1b[2J")) {
		panic("screen write failed")
	}
	return w.Buffer.Write(p)
}

// WriteString hides bytes.Buffer.WriteString, which io.WriteString would use.
func (w *panicWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestPluginInstallRestoresTerminalOnPanic(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	previous := installTerminal
	restored := 0
	installTerminal = func(io.Reader) (installChoice, func()) { return installRawKeys, func() { restored++ } }
	t.Cleanup(func() { installTerminal = previous })
	cmd := newRootCommand(t.TempDir())
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&panicWriter{})
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--state", store.Dir, "--log-level", "off", "plugins", "install", nativeInstallFolder(t)})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the screen did not panic")
			}
		}()
		_ = cmd.ExecuteContext(t.Context())
	}()
	if restored != 1 {
		t.Fatalf("terminal restored %d times after a panic, want 1", restored)
	}
	assertNothingInstalled(t, store.Dir)
}

func TestPluginInstallNeedsTerminalOrExplicitYes(t *testing.T) {
	store, session := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	if output, err := pluginInstallLocal(t, store.Dir, installNoTerminal, "y\n", folder); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatal("no-terminal install did not fail", output, err)
	}
	assertNothingInstalled(t, store.Dir)
	output, err := pluginInstallLocal(t, store.Dir, installNoTerminal, "", "--yes", "--allow-unsandboxed", "--tree-sha256", "0", "--session", session.ID, folder)
	tree := regexp.MustCompile(`snapshot ([0-9a-f]{64})`).FindStringSubmatch(err.Error())
	if tree == nil {
		t.Fatal("tree mismatch did not show the snapshot", output, err)
	}
	other := "codex:00000000-0000-4000-8000-0000000000ff"
	refused := []struct {
		want string
		args []string
	}{
		{"need --yes", []string{"--session", session.ID, folder}},
		{"needs --allow-unsandboxed", []string{"--yes", "--tree-sha256", tree[1], "--session", session.ID, folder}},
		{"Select at least one session for changes", []string{"--yes", "--allow-unsandboxed", "--tree-sha256", tree[1], folder}},
		{"unknown session", []string{"--yes", "--allow-unsandboxed", "--tree-sha256", tree[1], "--session", other, folder}},
		{"did not request", []string{"--yes", "--allow-unsandboxed", "--tree-sha256", tree[1], "--session", session.ID, "--send", session.ID, folder}},
		{"did not request", []string{"--yes", "--allow-unsandboxed", "--tree-sha256", tree[1], "--session", session.ID, "--accept-from", session.ID, folder}},
	}
	for _, c := range refused {
		output, err := pluginInstallLocal(t, store.Dir, installNoTerminal, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(output, "Enabling...") {
			t.Fatal("flags not refused for the expected reason", c.want, output, err)
		}
		assertNothingInstalled(t, store.Dir)
	}
	output, err = pluginInstallLocal(t, store.Dir, installNoTerminal, "", "--yes", "--allow-unsandboxed", "--tree-sha256", tree[1], "--session", session.ID, "--inbox-read", folder)
	if err != nil || !strings.Contains(output, "Accepted by --yes.") || !strings.Contains(output, "[x] changes") {
		t.Fatal(output, err)
	}
	if c := readInstallStatus(t, store.Dir, "demo").Capabilities; c == nil || !c.InboxRead || len(c.Read) != 0 {
		t.Fatal("--yes granted more or less than asked", c)
	}
}

func TestPluginInstallBuildFailureKeepsOnlyLog(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	store, _ := pluginOwnerFixture(t)
	manifest := `{"name":"broken","language":"go","purpose":"fails","build":["go","build","-o","plugin","./missing"],"artifact":"plugin","requests":{"feed":true,"read":false,"send":false,"inbox_read":false,"accept_from":false}}`
	folder := writeInstallFolder(t, manifest, map[string]string{"go.mod": "module example.com/broken\n\ngo 1.22\n"})
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, " y", folder)
	log := filepath.Join(store.Dir, "plugins", ".staging", "broken", "build.log")
	if err == nil || !strings.Contains(err.Error(), "Full log: "+log) || !strings.Contains(err.Error(), "Nothing was installed") {
		t.Fatal("build failure not reported", output, err)
	}
	if files := strings.Join(pluginStateFiles(t, store.Dir), " "); files != ".staging/ .staging/broken/ .staging/broken/build.log broken.lock" {
		t.Fatal("build failure state:", files)
	}
	// The next install of that name sweeps the log; so does plugins remove.
	if _, err := pluginInstallLocal(t, store.Dir, installRawKeys, "n", folder); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("next install kept the old log", err)
	}
	if _, err := pluginInstallLocal(t, store.Dir, installRawKeys, " y", folder); err == nil {
		t.Fatal("broken build installed")
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "remove", "broken"); err != nil {
		t.Fatal(string(raw), err)
	}
	if files := strings.Join(pluginStateFiles(t, store.Dir), " "); files != ".staging/ broken.lock" {
		t.Fatal("remove kept the build log:", files)
	}
}

func TestPluginInstallRefusesLinksAndBadInputs(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	linked := nativeInstallFolder(t)
	if err := os.Symlink(outside, filepath.Join(linked, "link")); err != nil {
		t.Fatal(err)
	}
	linkArtifact := nativeInstallFolder(t)
	if err := os.Remove(filepath.Join(linkArtifact, "bin", "plugin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkArtifact, "bin", "plugin")); err != nil {
		t.Fatal(err)
	}
	fifo := nativeInstallFolder(t)
	if err := syscall.Mkfifo(filepath.Join(fifo, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := writeInstallFolder(t, nativeInstallManifest, map[string]string{})
	notExec := writeInstallFolder(t, nativeInstallManifest, map[string]string{"bin/plugin": "data"})
	hostile := writeInstallFolder(t, strings.Replace(nativeInstallManifest, "test notices", "a\\u001b[2Jb", 1), map[string]string{"bin/plugin": "#!/bin/sh\n"})
	cases := []struct{ folder, want string }{
		{linked, "links and devices are not allowed"},
		{linkArtifact, "links and devices are not allowed"},
		{fifo, "links and devices are not allowed"},
		{missing, "was not built"},
		{notExec, "executable regular file"},
		{hostile, "purpose must be plain text"},
	}
	for _, c := range cases {
		output, err := pluginInstallLocal(t, store.Dir, installRawKeys, " y", c.folder)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(output, "Installed and enabled") {
			t.Fatal(c.want, "case was accepted or failed for another reason:", output, err)
		}
		assertNothingInstalled(t, store.Dir)
	}
	// Abandoned data is never adopted.
	if err := os.MkdirAll(filepath.Join(store.Dir, "plugins", "demo.data"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "plugins", "demo.data", "old"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pluginInstallLocal(t, store.Dir, installRawKeys, " y", nativeInstallFolder(t)); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatal("abandoned data adopted", err)
	}
}

func TestPluginInstallScreenEscapesText(t *testing.T) {
	screen := newInstallScreen("h\n", plugins.InstallRequests{Feed: true}, []model.Session{{ID: "codex:x", Name: "evil\x1b[2J\u202e"}})
	if out := screen.render(screenKeys); strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\u202e") {
		t.Fatalf("unescaped session name: %q", out)
	}
	if plainText("plain path") != "plain path" || plainText("a\tb") != `"a\tb"` {
		t.Fatal("plainText changed plain text or kept a tab")
	}
}

func TestPluginInstallScreenLimitsAndNoSessions(t *testing.T) {
	sessions := make([]model.Session, 17)
	for i := range sessions {
		sessions[i].ID = "codex:" + strings.Repeat("x", i+1)
	}
	screen := newInstallScreen("", plugins.InstallRequests{Feed: true}, sessions)
	for i := range sessions {
		screen.selected[[2]int{i, 0}] = true
	}
	if !strings.Contains(screen.check(), "at most 16") {
		t.Fatal("17 sessions accepted")
	}
	state := filepath.Join(t.TempDir(), "state")
	output, err := pluginInstallLocal(t, state, installRawKeys, " y", nativeInstallFolder(t))
	if err == nil || !strings.Contains(err.Error(), "no Zalki sessions") {
		t.Fatal("install without sessions did not explain", output, err)
	}
	assertNothingInstalled(t, state)
}

func TestPluginInstallHidesRetiredSessions(t *testing.T) {
	store, session := pluginOwnerFixture(t)
	retired, _, err := store.Register("codex", "00000000-0000-4000-8000-000000000009", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(retired.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "n", nativeInstallFolder(t))
	if err != nil || !strings.Contains(output, session.ID) || strings.Contains(output, retired.ID) {
		t.Fatal("retired session offered", output, err)
	}
}

func TestPluginInstallCommitErrorNeverHidesAKeptRecord(t *testing.T) {
	failed := errors.New("injected")
	if installCommitError("demo")(false, nil) != nil {
		t.Fatal("success reported as an error")
	}
	kept := installCommitError("demo")(true, failed).Error()
	if strings.Contains(kept, "Nothing was installed") || !strings.Contains(kept, "may be installed and enabled") || !strings.Contains(kept, "zalki plugins status demo") {
		t.Fatal("kept record hidden:", kept)
	}
	if rolled := installCommitError("demo")(false, failed).Error(); !strings.Contains(rolled, "Nothing was installed") {
		t.Fatal("rollback not reported:", rolled)
	}
}
