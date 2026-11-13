package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

func pluginUpdateLocal(t *testing.T, state string, mode installChoice, input string, args ...string) (string, error) {
	t.Helper()
	previous := installTerminal
	installTerminal = func(io.Reader) (installChoice, func()) { return mode, func() {} }
	defer func() { installTerminal = previous }()
	cmd := newRootCommand(configTestHome(t))
	output := &cappedOutput{limit: 4 << 20}
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(output)
	cmd.SetErr(output)
	cmd.SetArgs(append([]string{"--state", state, "--log-level", "off", "plugins", "update"}, args...))
	err := cmd.ExecuteContext(t.Context())
	return output.String(), err
}

func installedUpdateFolder(t *testing.T, name string) (string, string, installStatus) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	args := []string{folder}
	if name != "demo" {
		args = append(args, "--as", name)
	}
	if output, err := pluginInstallLocal(t, store.Dir, installPlainLines, "y\n", args...); err != nil {
		t.Fatal(output, err)
	}
	return store.Dir, folder, readInstallStatus(t, store.Dir, name)
}

func changeUpdateFolder(t *testing.T, folder string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(folder, "bin/plugin"), []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestPluginUpdateEnabledAndAliasKeepsData(t *testing.T) {
	state, folder, old := installedUpdateFolder(t, "demo-two")
	data := filepath.Join(old.DataDir, "settings.json")
	if err := os.WriteFile(data, []byte("private settings"), 0600); err != nil {
		t.Fatal(err)
	}
	changeUpdateFolder(t, folder)
	output, err := pluginUpdateLocal(t, state, installPlainLines, "y\n", "demo-two")
	if err != nil {
		t.Fatal(output, err)
	}
	now := readInstallStatus(t, state, "demo-two")
	got, err := os.ReadFile(data)
	if err != nil || string(got) != "private settings" || now.ID != old.ID || !now.Enabled || now.Epoch == old.Epoch || now.Revision == old.Revision || now.Install.InstalledBy != "terminal" {
		t.Fatal("enabled alias update lost data, identity, binding or provenance", err)
	}
	if strings.Count(output, updateQuestion) != 1 || !strings.Contains(output, "Updated demo-two:") {
		t.Fatal("update added a confirmation or lost result", output)
	}
	prefix, _, found := strings.Cut(output, "Verifying...")
	if !found {
		t.Fatal("update did not reach verification", output)
	}
	lines := strings.Split(strings.TrimSpace(prefix), "\n")
	if len(lines) != 9 || strings.Index(output, "STATUS:") > strings.Index(output, "Source:") {
		t.Fatal("update screen exceeds nine lines or diff is not first", output)
	}
}

func TestPluginUpdateUpToDateDoesNotWriteRecord(t *testing.T) {
	state, _, _ := installedUpdateFolder(t, "demo")
	path := filepath.Join(state, "plugins", "demo.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := pluginUpdateLocal(t, state, installNoTerminal, "", "demo")
	after, readErr := os.ReadFile(path)
	now, statErr := os.Stat(path)
	if err != nil || readErr != nil || statErr != nil || string(before) != string(after) || !now.ModTime().Equal(info.ModTime()) || strings.Count(output, "\n") != 1 || !strings.Contains(output, "up to date") {
		t.Fatal("unchanged update wrote the record or displayed a screen", output, err)
	}
}

func TestPluginUpdateNewAccessStartsUnselected(t *testing.T) {
	state, folder, _ := installedUpdateFolder(t, "demo")
	path := filepath.Join(folder, "zalki-plugin.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Replace RECEIVE with a newly requested READ; keeping the old SEND/READ
	// grants must never turn an author's declaration into owner permission.
	raw = []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), `"receive":true`, `"receive":false`), `"read":false`, `"read":true`))
	mustWriteInstallManifest(t, folder, raw)
	output, err := pluginUpdateLocal(t, state, installPlainLines, "\ny\n", "demo")
	if err != nil {
		t.Fatal(output, err)
	}
	now := readInstallStatus(t, state, "demo")
	if now.Access.Receive || len(now.Access.Read) != 0 || !strings.Contains(output, "NEW request; starts unselected") || !strings.Contains(output, "RECEIVE: true -> false (scope all sessions, now and later) (removed)") {
		t.Fatal("new request was granted or removed access was hidden", output, now.Access)
	}
}

func TestPluginUpdateNewStandardAccessRequiresSelection(t *testing.T) {
	state, folder, _ := installedUpdateFolder(t, "demo")
	if _, err := pluginOwnerLocal(t, state, "grant", "demo", "--status"); err != nil {
		t.Fatal(err)
	}
	// Receive was requested before, but declined; it must remain declined.
	changeUpdateFolder(t, folder)
	output, err := pluginUpdateLocal(t, state, installPlainLines, "y\n", "demo")
	if err != nil {
		t.Fatal(output, err)
	}
	if readInstallStatus(t, state, "demo").Access.Receive {
		t.Fatal("update silently restored declined standard access")
	}
	changeUpdateFolder(t, folder)
	if err := os.WriteFile(filepath.Join(folder, "extra"), []byte("next snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err = pluginUpdateLocal(t, state, installPlainLines, "r\ny\n", "demo")
	if err != nil || !readInstallStatus(t, state, "demo").Access.Receive {
		t.Fatal("same-question standard toggle failed", output, err)
	}
}

func TestPluginUpdateYesAndTerminalGuards(t *testing.T) {
	state, folder, old := installedUpdateFolder(t, "demo")
	changeUpdateFolder(t, folder)
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	output, err := pluginUpdateLocal(t, state, installNoTerminal, "y\n", "demo")
	if err == nil || !strings.Contains(err.Error(), "own terminal") || readInstallStatus(t, state, "demo").Install.TreeSHA256 != old.Install.TreeSHA256 {
		t.Fatal("piped update bypassed input terminal", output, err)
	}
	output, err = pluginUpdateLocal(t, state, installNoTerminal, "", "demo", "--yes", "--tree-sha256", snapshot.TreeSHA256, "--allow-unsandboxed")
	if err != nil || !strings.Contains(output, "Accepted by --yes") {
		t.Fatal(output, err)
	}
	raw, err := os.ReadFile(filepath.Join(folder, "zalki-plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteInstallManifest(t, folder, []byte(strings.ReplaceAll(string(raw), `"status":true`, `"status":false`)))
	snapshot, err = plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = pluginUpdateLocal(t, state, installNoTerminal, "", "demo", "--yes", "--tree-sha256", snapshot.TreeSHA256, "--allow-unsandboxed")
	if err == nil || !strings.Contains(err.Error(), "unchanged declared and granted standard access") {
		t.Fatal("--yes accepted a changed access declaration", err)
	}
}

func TestPluginUpdateCancelAndMovedSource(t *testing.T) {
	state, folder, old := installedUpdateFolder(t, "demo")
	changeUpdateFolder(t, folder)
	output, err := pluginUpdateLocal(t, state, installRawKeys, "n", "demo")
	if err != nil || !strings.Contains(output, "old version is still installed") || readInstallStatus(t, state, "demo").Install.TreeSHA256 != old.Install.TreeSHA256 {
		t.Fatal("cancel changed the installation", output, err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(folder, moved); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(moved)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pluginUpdateLocal(t, state, installNoTerminal, "", "demo")
	if err == nil || !strings.Contains(err.Error(), "zalki plugins update demo NEW_FOLDER") {
		t.Fatal("missing source lacks exact recovery command", err)
	}
	output, err = pluginUpdateLocal(t, state, installPlainLines, "y\n", "demo", moved)
	if err != nil || !strings.Contains(output, "(moved)") || readInstallStatus(t, state, "demo").Install.Source != resolved {
		t.Fatal("moved source was not recorded", output, err)
	}
}

// Recursive inventory includes directory and file mtimes, modes, symlink
// targets and file bytes. Atime is intentionally excluded: reads may change it.
func updateStateInventory(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return result
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%s %d", info.Mode(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += fmt.Sprintf(" %x", sha256.Sum256(raw))
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += " " + target
		}
		result[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func runUpdateBinary(t *testing.T, binary, state string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary, append([]string{"--state", state, "--log-level", "debug"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return string(out), exit.ExitCode()
}

func TestPluginUpdateCheckExitCodes(t *testing.T) {
	binary := checkRealBinary(t)
	state, folder, _ := installedUpdateFolder(t, "demo")
	prepareUpdateReadOnlyState(t, state)
	before := updateStateInventory(t, state)
	for _, code := range []int{0, 2} {
		if code == 2 {
			changeUpdateFolder(t, folder)
		}
		output, got := runUpdateBinary(t, binary, state, "plugins", "update", "demo", "--check")
		if got != code {
			t.Fatal("wrong update check exit code", got, output)
		}
		if !reflect.DeepEqual(before, updateStateInventory(t, state)) {
			t.Fatal("--check wrote state (including mtimes)")
		}
	}
	output, code := runUpdateBinary(t, binary, state, "plugins", "update", "absent", "--check")
	if code != 1 || !strings.Contains(output, "is not installed") || !reflect.DeepEqual(before, updateStateInventory(t, state)) {
		t.Fatal("failing --check touched log/recovery state", output, code)
	}
	missing := filepath.Join(t.TempDir(), "no-state")
	output, code = runUpdateBinary(t, binary, missing, "plugins", "update", "demo", "--check")
	if code != 1 || !strings.Contains(output, "is not installed") {
		t.Fatal(output, code)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("--check created missing state", err)
	}
}

func TestPluginInstallDryRun(t *testing.T) {
	binary := checkRealBinary(t)
	state, folder, _ := installedUpdateFolder(t, "demo")
	log := filepath.Join(state, "protected.log")
	if err := os.WriteFile(log, []byte("private log sentinel\n"), 0400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_LOG_FILE", log)
	t.Setenv("ZALKI_LOG_LEVEL", "debug")
	for _, dir := range []string{state, filepath.Join(t.TempDir(), "no-state")} {
		before := updateStateInventory(t, dir)
		output, code := runUpdateBinary(t, binary, dir, "plugins", "install", "--dry-run", folder)
		if code != 0 || !strings.Contains(output, "Dry run passed:") {
			t.Fatal(output, code)
		}
		if !reflect.DeepEqual(before, updateStateInventory(t, dir)) {
			t.Fatal("dry-run wrote state (including mtimes)")
		}
		if _, err := os.Stat(dir); len(before) == 0 && !errors.Is(err, os.ErrNotExist) {
			t.Fatal("dry-run created missing state", err)
		}
		before = updateStateInventory(t, dir)
		output, code = runUpdateBinary(t, binary, dir, "plugins", "install", "--dry-run", filepath.Join(folder, "missing"))
		if code != 1 || !reflect.DeepEqual(before, updateStateInventory(t, dir)) {
			t.Fatal("failing dry-run wrote state/log", output, code)
		}
	}
}

func TestPluginUpdateCheckDoesNotWriteDebugLog(t *testing.T) {
	binary := checkRealBinary(t)
	state, _, _ := installedUpdateFolder(t, "demo")
	log := filepath.Join(state, "debug.log")
	if err := os.WriteFile(log, []byte("existing log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_LOG_FILE", log)
	t.Setenv("ZALKI_LOG_LEVEL", "debug")
	before := updateStateInventory(t, state)
	for _, name := range []string{"demo", "absent"} {
		output, code := runUpdateBinary(t, binary, state, "plugins", "update", name, "--check")
		want := 0
		if name == "absent" {
			want = 1
		}
		if code != want {
			t.Fatal("write-free check attempted root logger", output, code)
		}
		if !reflect.DeepEqual(before, updateStateInventory(t, state)) {
			t.Fatal("write-free check wrote inherited debug log")
		}
	}
}

func TestPluginInstallDryRunDoesNotWriteDebugLog(t *testing.T) {
	binary := checkRealBinary(t)
	state, folder, _ := installedUpdateFolder(t, "demo")
	log := filepath.Join(state, "debug.log")
	if err := os.WriteFile(log, []byte("existing log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_LOG_FILE", log)
	t.Setenv("ZALKI_LOG_LEVEL", "debug")
	before := updateStateInventory(t, state)
	for _, source := range []string{folder, filepath.Join(folder, "missing")} {
		output, code := runUpdateBinary(t, binary, state, "plugins", "install", "--dry-run", source)
		want := 0
		if source != folder {
			want = 1
		}
		if code != want {
			t.Fatal("write-free dry-run attempted root logger", output, code)
		}
		if !reflect.DeepEqual(before, updateStateInventory(t, state)) {
			t.Fatal("write-free dry-run wrote inherited debug log")
		}
	}
}

func TestPluginUpdateYesRefusesChangedGrantedAccess(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	manifest := strings.ReplaceAll(nativeInstallManifest, `"status":true`, `"status":false`)
	folder := writeInstallFolder(t, manifest, map[string]string{"bin/plugin": "#!/bin/sh\nexit 0\n"})
	if output, err := pluginInstallLocal(t, store.Dir, installPlainLines, "y\n", folder); err != nil {
		t.Fatal(output, err)
	}
	if _, err := pluginOwnerLocal(t, store.Dir, "grant", "demo", "--status", "--receive"); err != nil {
		t.Fatal(err)
	}
	changeUpdateFolder(t, folder)
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = pluginUpdateLocal(t, store.Dir, installNoTerminal, "", "demo", "--yes", "--tree-sha256", snapshot.TreeSHA256, "--allow-unsandboxed")
	if err == nil || !strings.Contains(err.Error(), "unchanged declared and granted standard access") {
		t.Fatal("--yes accepted changed granted standard access", err)
	}
}

func TestPluginUpdateReviewScreenGrowth(t *testing.T) {
	p := &pluginInstaller{name: "demo", folder: "/src/demo", update: &pluginfeed.PluginFeedStatus{Enabled: true, Scope: "all", Install: &pluginfeed.PluginInstallReceipt{Source: "/src/demo", TreeSHA256: strings.Repeat("a", 64), ArtifactSHA256: strings.Repeat("c", 64), Access: json.RawMessage(`{"status":true,"receive":true,"send":false,"read":false}`)}}}
	p.source = plugins.InstallSource{TreeSHA256: strings.Repeat("b", 64), Manifest: plugins.InstallManifest{ID: "local/demo", Version: "1.0", Artifact: "plugin", Access: plugins.InstallAccess{Status: true, Receive: true, Send: true, Read: true}}}
	lines, err := p.updateReviewLines(p.retainedUpdateAccess(), nil)
	if err != nil || len(lines) > 9 || !strings.Contains(strings.Join(lines, "\n"), "NEW request; starts unselected") {
		t.Fatal("unsafe or oversized update review", lines, err)
	}
}

func prepareUpdateReadOnlyState(t *testing.T, state string) {
	t.Helper()
	log := filepath.Join(state, "protected.log")
	if err := os.WriteFile(log, []byte("private log sentinel\n"), 0400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_LOG_FILE", log)
	t.Setenv("ZALKI_LOG_LEVEL", "debug")
	if err := os.MkdirAll(filepath.Join(state, "commits"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "commits/pending.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "transactions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "transactions/00000000-0000-4000-8000-000000000099.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPluginUpdateAvailableJoinedCleanupIsFailure(t *testing.T) {
	if commandExitCode(errPluginUpdateAvailable) != 2 {
		t.Fatal("available marker lost exit2")
	}
	err := errors.Join(errPluginUpdateAvailable, errors.New("controlled cleanup failure"))
	if commandExitCode(err) != 1 {
		t.Fatal("available marker hid a real cleanup failure")
	}
}

func TestPluginUpdateYesReviewRefusesChangedAccess(t *testing.T) {
	for _, kind := range []string{"declaration", "grant"} {
		t.Run(kind, func(t *testing.T) {
			declared := plugins.InstallAccess{Status: true, Receive: true}
			m := declared
			if kind == "grant" {
				declared.Status = false
			}
			m.Status = false
			raw, err := json.Marshal(declared)
			if err != nil {
				t.Fatal(err)
			}
			hash := strings.Repeat("a", 64)
			p := &pluginInstaller{cmd: newRootCommand(t.TempDir()), name: "demo", folder: "/src/demo", flags: installFlags{yes: true, tree: hash, allowUnsandboxed: true}, update: &pluginfeed.PluginFeedStatus{Scope: "all", Access: pluginfeed.PluginAccess{Status: true, Receive: true}, Install: &pluginfeed.PluginInstallReceipt{Access: raw, Source: "/src/demo", TreeSHA256: hash, ArtifactSHA256: hash}}}
			p.source = plugins.InstallSource{TreeSHA256: hash, Manifest: plugins.InstallManifest{ID: "local/demo", Version: "1", Artifact: "plugin", Access: m}}
			p.cmd.SetOut(io.Discard)
			_, ok, err := p.reviewUpdateYes(p.retainedUpdateAccess())
			if err == nil || ok {
				t.Fatal("--yes review approved changed access", kind, err)
			}
		})
	}
}

// Exercise the selection policy directly: renderer/backend refusals must not
// conceal an update controller retaining an author-unrequested grant.
func TestPluginUpdateRetainedAccessIntersection(t *testing.T) {
	id := "codex:00000000-0000-4000-8000-000000000001"
	p := &pluginInstaller{update: &pluginfeed.PluginFeedStatus{Access: pluginfeed.PluginAccess{Status: true, Receive: true, Send: []string{id}, Read: []string{id}}, Install: &pluginfeed.PluginInstallReceipt{Access: json.RawMessage(`{"status":true,"receive":true,"send":true,"read":true}`)}}}
	p.source.Manifest.Access = plugins.InstallAccess{Status: true, Receive: true}
	a := p.retainedUpdateAccess()
	if len(a.Send) != 0 {
		t.Fatal("update retained SEND no longer requested")
	}
	if len(a.Read) != 0 {
		t.Fatal("update retained READ no longer requested")
	}
	if !a.Status || !a.Receive {
		t.Fatal("update lost retained standard access")
	}
}
