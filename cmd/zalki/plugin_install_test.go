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
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

const nativeInstallManifest = `{"api":"zalki-plugin/1","id":"local/demo","version":"0.1.0","name":"demo","language":"native","purpose":"test notices","build":null,"artifact":"bin/plugin","access":{"status":true,"receive":true,"send":false,"read":false}}`

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
	defer func() { installTerminal = previous }()
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
	store, _ := pluginOwnerFixture(t)
	manifest := `{"api":"zalki-plugin/1","id":"local/gonote","version":"0.1.0","name":"gonote","language":"go","purpose":"desktop notices","build":["go","build","-mod=readonly","-o","plugin","."],"artifact":"plugin","args":["--quiet"],"access":{"status":true,"receive":false,"send":false,"read":false}}`
	folder := writeInstallFolder(t, manifest, map[string]string{
		"go.mod":  "module example.com/gonote\n\ngo 1.22\n",
		"main.go": "package main\n\nimport (\"io\"; \"os\")\n\nfunc main() { _, _ = io.Copy(io.Discard, os.Stdin) }\n",
	})
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", folder, "--details")
	if err != nil {
		t.Fatal(output, err)
	}
	for _, want := range []string{"Install \"gonote\" (local/gonote 0.1.0, author's claim)\n", "Build: [\"go\", \"build\", \"-mod=readonly\", \"-o\", \"plugin\", \".\"]\nTool: /", "(go launcher; its hash is checked for changes, the compiler is not pinned)", "Tool SHA256: ", "Build env: only PATH from your environment; private caches, GOTOOLCHAIN=local, GOENV=off, GOWORK=off, -buildvcs=false, -mod=readonly (vendor/ is ignored), cgo off, no network (GOPROXY=off), 2 CPUs. go.mod may use only the standard library. VCS folders such as .git are not copied.\n", "  see the status of all sessions, now and later\n", "Artifact: plugin; artifact pin: pending build\nRun arguments: [\"--quiet\"]\n", "Publisher proof: none", "no sandbox", "Building...", "Installed. Start it with: zalki plugins run gonote"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	tree := assertGoInstallStatus(t, store.Dir, output)
	assertInstalledLifecycle(t, store.Dir, tree)
}

func assertGoInstallStatus(t *testing.T, state, output string) string {
	t.Helper()
	status := readInstallStatus(t, state, "gonote")
	bin := filepath.Join(state, "plugins", "gonote.bin", status.Launch.SHA256)
	if !status.Enabled || status.Scope != "all" || !status.Access.Status || status.Access.Receive || status.PinMatches == nil || !*status.PinMatches || status.Launch.Executable != bin || !slices.Equal(status.Launch.Args, []string{"--quiet"}) {
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
func assertInstalledLifecycle(t *testing.T, state, tree string) {
	t.Helper()
	for _, args := range [][]string{{"disable", "gonote"}, {"configure", "gonote", "--scope", "all"}, {"grant", "gonote", "--status"}, {"enable", "gonote"}} {
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
	store, _ := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	assertInstallCancels(t, store.Dir, folder)
	// Standard access needs only the one confirmation.
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", folder)
	if err != nil {
		t.Fatal(output, err)
	}
	assertNativeChoices(t, store.Dir, output)
	// A second install of the same name stops before the screen.
	if output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", folder); err == nil || !strings.Contains(err.Error(), "already installed") || strings.Contains(output, `Install "demo"`) {
		t.Fatal("reinstall was not refused early", output, err)
	}
	assertSecondInstance(t, store.Dir, folder)
}

// --as installs one more instance of the same plugin id: its own record,
// installation, address, data folder and lease. Only a NAME is unique.
func assertSecondInstance(t *testing.T, state, folder string) {
	t.Helper()
	output, err := pluginInstallLocal(t, state, installRawKeys, "y", folder, "--as", "demo-2")
	if err != nil || !strings.Contains(output, `Install "demo-2" (local/demo 0.1.0, author's claim)`) || !strings.Contains(output, "Installed. Start it with: zalki plugins run demo-2") {
		t.Fatal("second instance", output, err)
	}
	first, second := readInstallStatus(t, state, "demo"), readInstallStatus(t, state, "demo-2")
	if first.ID == second.ID || first.DataDir == second.DataDir || second.Install == nil || second.Install.ID != "local/demo" || first.Install.ID != second.Install.ID || second.Install.API != "zalki-plugin/1" || second.Install.Version != "0.1.0" {
		t.Fatalf("instances share state: %+v %+v", first, second)
	}
	for _, name := range []string{"demo", "demo-2"} {
		if _, err := os.Stat(filepath.Join(state, "plugins", name+".lock")); err != nil {
			t.Fatal("each instance needs its own lease", name, err)
		}
	}
	if output, err := pluginInstallLocal(t, state, installRawKeys, "y", folder, "--as", "demo-2"); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Fatal("the same NAME twice was not refused", output, err)
	}
}

func assertNativeChoices(t *testing.T, state, output string) {
	t.Helper()
	status := readInstallStatus(t, state, "demo")
	if a := status.Access; !status.Enabled || status.Scope != "all" || !a.Status || !a.Receive || len(a.Read)+len(a.Send) != 0 || status.Revision == "" {
		t.Fatalf("access not recorded exactly: %+v", status)
	}
	if status.Install.Tool != "" || len(status.Install.Build) != 0 || status.Install.Build == nil {
		t.Fatal("native receipt claimed a build", status.Install)
	}
	if !strings.Contains(output, `Ready-made artifact; runs: ["bin/plugin"]`) {
		t.Fatal(output)
	}
}

func assertInstallCancels(t *testing.T, state, folder string) {
	t.Helper()
	for _, input := range []string{"n", "\ry", "q", "\x03y", "", " ", "\x1b[Cy", "\x1by"} {
		output, err := pluginInstallLocal(t, state, installRawKeys, input, folder)
		if err != nil || !strings.Contains(output, "Cancelled. Nothing was installed.") {
			t.Fatalf("input %q: %s %v", input, output, err)
		}
		assertNothingInstalled(t, state)
	}
}

func TestPluginInstallPlainLineFallback(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	output, err := pluginInstallLocal(t, store.Dir, installPlainLines, "y\n", folder)
	if err != nil {
		t.Fatal(output, err)
	}
	for _, want := range []string{"  see the status of all sessions, now and later", installQuestion, "Installed. Start it with: zalki plugins run demo"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	if status := readInstallStatus(t, store.Dir, "demo"); status.Scope != "all" || !status.Access.Status || !status.Access.Receive {
		t.Fatal("fallback recorded wrong access", status)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "disable", "demo"); err != nil {
		t.Fatal(string(raw), err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "remove", "demo"); err != nil {
		t.Fatal(string(raw), err)
	}
	for input, want := range map[string]string{
		"n\n":    "Cancelled. Nothing was installed.",
		"\n":     "Cancelled. Nothing was installed.",
		"":       "Cancelled. Nothing was installed.",
		"yes!\n": "Cancelled. Nothing was installed.",
		"y":      "Cancelled. Nothing was installed.", // closed input after a final y is No
	} {
		output, err := pluginInstallLocal(t, store.Dir, installPlainLines, input, folder)
		if err != nil {
			output += err.Error()
		}
		if strings.Contains(output, "Installed. Start it with:") || !strings.Contains(output, want) {
			t.Fatalf("input %q: want %q, got: %s", input, want, output)
		}
		assertNothingInstalled(t, store.Dir)
	}
}

func TestPluginInstallReadsConfirmationKeys(t *testing.T) {
	for input, want := range map[string]pluginReviewAnswer{
		"y": pluginReviewAccept, "Y": pluginReviewAccept, "\x1b[A": pluginReviewCancel,
		"\x1by": pluginReviewCancel, "\r": pluginReviewCancel, "\x03": pluginReviewCancel,
		"": pluginReviewCancel,
	} {
		got, err := readPluginReviewAnswer(bufio.NewReader(strings.NewReader(input)), true)
		if err != nil || got != want {
			t.Fatalf("input %q: got %v err %v want %v", input, got, err, want)
		}
	}
}

// The one question reads one key: only y installs. ESC, Enter or an arrow
// cancels, and a y typed after them is not taken as the answer.
func TestPluginInstallRawEscCancels(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "\x1by", nativeInstallFolder(t))
	if err != nil || !strings.Contains(output, "Cancelled. Nothing was installed.") {
		t.Fatalf("ESC did not cancel: %v\n%s", err, output)
	}
	assertNothingInstalled(t, store.Dir)
}

// panicWriter panics when the raw screen is drawn.
type panicWriter struct{ bytes.Buffer }

func (w *panicWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(installQuestion)) {
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
	store, _ := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	if output, err := pluginInstallLocal(t, store.Dir, installNoTerminal, "y\n", folder); err == nil || !strings.Contains(err.Error(), "Run this in your own terminal") {
		t.Fatal("no-terminal install did not fail", output, err)
	}
	assertNothingInstalled(t, store.Dir)
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tree := []string{"", snapshot.TreeSHA256}
	var output string
	refused := []struct {
		want string
		args []string
	}{
		{"need --yes", []string{"--allow-unsandboxed", folder}},
		{"add --allow-unsandboxed", []string{"--yes", "--tree-sha256", tree[1], folder}},
		{"unknown flag: --session", []string{"--yes", "--allow-unsandboxed", "--tree-sha256", tree[1], "--session", "codex:x", folder}},
		{"unknown flag: --send", []string{"--yes", "--allow-unsandboxed", "--tree-sha256", tree[1], "--send", "codex:x", folder}},
	}
	for _, c := range refused {
		output, err := pluginInstallLocal(t, store.Dir, installNoTerminal, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(output, "Enabling...") {
			t.Fatal("flags not refused for the expected reason", c.want, output, err)
		}
		assertNothingInstalled(t, store.Dir)
	}
	output, err = pluginInstallLocal(t, store.Dir, installNoTerminal, "", "--yes", "--allow-unsandboxed", "--tree-sha256", tree[1], folder)
	if err != nil || !strings.Contains(output, "Accepted by --yes (standard access only).") || !strings.Contains(output, "see the status of all sessions") {
		t.Fatal(output, err)
	}
	if s := readInstallStatus(t, store.Dir, "demo"); s.Scope != "all" || !s.Access.Status || !s.Access.Receive || len(s.Access.Read)+len(s.Access.Send) != 0 {
		t.Fatal("--yes granted more or less than the manifest's standard access", s)
	}
}

// A manifest that asks for send or read is not installed here: the error
// names the owner-terminal commands that give that access.
func TestPluginInstallRefusesExtendedAccess(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	manifest := strings.Replace(nativeInstallManifest, `"read":false`, `"read":true`, 1)
	folder := writeInstallFolder(t, manifest, map[string]string{"bin/plugin": "#!/bin/sh\nexit 0\n"})
	output, err := pluginInstallLocal(t, store.Dir, installNoTerminal, "", "--yes", folder)
	if err == nil || !strings.Contains(err.Error(), "asks for extended access (READ). Run this in your own terminal: zalki plugins install ") || strings.Contains(output, installQuestion) {
		t.Fatal("extended --yes did not refuse with its terminal command", output, err)
	}
	assertNothingInstalled(t, store.Dir)
}

func TestPluginInstallBuildFailureKeepsOnlyLog(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	store, _ := pluginOwnerFixture(t)
	manifest := `{"api":"zalki-plugin/1","id":"local/broken","version":"0.1.0","name":"broken","language":"go","purpose":"fails","build":["go","build","-o","plugin","./missing"],"artifact":"plugin","access":{"status":true,"receive":false,"send":false,"read":false}}`
	folder := writeInstallFolder(t, manifest, map[string]string{"go.mod": "module example.com/broken\n\ngo 1.22\n"})
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", folder)
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
	if _, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", folder); err == nil {
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
		output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", c.folder)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(output, "Installed. Start it with:") {
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
	if _, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", nativeInstallFolder(t)); err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatal("abandoned data adopted", err)
	}
}

func TestPluginInstallPlainTextEscapes(t *testing.T) {
	if plainText("plain path") != "plain path" || plainText("a\tb") != `"a\tb"` || plainText("evil\x1b[2J\u202e") == "evil\x1b[2J\u202e" {
		t.Fatal("plainText changed plain text or kept a control character")
	}
}

// Scope all needs no session at install, and the screen lists none: a session
// that starts later is in scope at once.
func TestPluginInstallNeedsNoSessionAndListsNone(t *testing.T) {
	store, session := pluginOwnerFixture(t)
	if err := store.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	output, err := pluginInstallLocal(t, store.Dir, installRawKeys, "y", nativeInstallFolder(t))
	if err != nil || !strings.Contains(output, "Installed. Start it with: zalki plugins run demo") || strings.Contains(output, session.ID) {
		t.Fatal("install without a live session failed or listed a session", output, err)
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
