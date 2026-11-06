package install_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/install"
)

// These tests describe the configuration path as an installer user sees it:
// the declarations of the built-in clients go in, files on disk come out. They
// adapt declarations only in configDeclare, using a shallow field-by-field
// conversion equivalent to today's direct installer input. They use temporary
// homes only; the CLI tests exercise the actual command wiring.

// configDeclare turns the built-in registry's declarations into the installer's
// execution specification. The JSON maps are passed on by reference, as the
// direct registry-to-installer wiring does today.
func configDeclare(binary, state string) ([]install.ConfigFile, error) {
	files, err := adapters.Builtin().Configuration(binary, state)
	if err != nil {
		return nil, err
	}
	out := make([]install.ConfigFile, 0, len(files))
	for _, file := range files {
		out = append(out, install.ConfigFile{Path: file.Path, Draft: file.Draft, JSON: file.JSON, TOML: file.TOML, ConfigRootEnv: file.ConfigRootEnv})
	}
	return out, nil
}

func isolateConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
}

// wantTargets lists where each declared file is written when no environment
// variable relocates it.
func wantTargets(home string) map[string]string {
	return map[string]string{
		".claude.json":          filepath.Join(home, ".claude.json"),
		".claude/settings.json": filepath.Join(home, ".claude", "settings.json"),
		".codex/config.toml":    filepath.Join(home, ".codex", "config.toml"),
		".codex/hooks.json":     filepath.Join(home, ".codex", "hooks.json"),
		".cursor/hooks.json":    filepath.Join(home, ".cursor", "hooks.json"),
		".cursor/mcp.json":      filepath.Join(home, ".cursor", "mcp.json"),
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestSetupWritesExactlyTheDeclaredDrafts(t *testing.T) {
	const binary = "/opt/my tools/zalki"
	drafts := []string{"claude-hooks.json", "claude-mcp.json", "codex-hooks.json", "codex-mcp.toml", "cursor-hooks.json", "cursor-mcp.json", "targets.json"}

	t.Run("default destinations", func(t *testing.T) {
		isolateConfigEnv(t)
		home := t.TempDir()
		state, out := filepath.Join(home, "state"), filepath.Join(home, "drafts")
		if err := install.WriteSetup(configDeclare, binary, state, out, home); err != nil {
			t.Fatal(err)
		}
		if got := entryNames(t, out); !reflect.DeepEqual(got, drafts) {
			t.Fatalf("drafts = %v, want %v", got, drafts)
		}
		if mode := modeOf(t, out); mode != 0700 {
			t.Errorf("draft directory mode %v, want 0700", mode)
		}
		for _, name := range drafts {
			if mode := modeOf(t, filepath.Join(out, name)); mode != 0600 {
				t.Errorf("%s: mode %v, want 0600", name, mode)
			}
		}
		// A JSON draft is the patch with two-space indentation and a final newline.
		wantCursorMCP := "{\n  \"mcpServers\": {\n    \"zalki\": {\n      \"args\": [\n        \"--state\",\n        \"" + state + "\",\n        \"mcp\"\n      ],\n      \"command\": \"" + binary + "\"\n    }\n  }\n}\n"
		if got := readFileString(t, filepath.Join(out, "cursor-mcp.json")); got != wantCursorMCP {
			t.Errorf("cursor-mcp.json differs\n got %q\nwant %q", got, wantCursorMCP)
		}
		// A TOML draft is the declared fragment, byte for byte.
		wantTOML := "\n[mcp_servers.zalki]\ncommand = \"" + binary + "\"\nargs = [\"--state\",\"" + state + "\",\"mcp\"]\n"
		if got := readFileString(t, filepath.Join(out, "codex-mcp.toml")); got != wantTOML {
			t.Errorf("codex-mcp.toml differs\n got %q\nwant %q", got, wantTOML)
		}
		var targets map[string]string
		if err := json.Unmarshal([]byte(readFileString(t, filepath.Join(out, "targets.json"))), &targets); err != nil {
			t.Fatal(err)
		}
		wantTargetsByDraft := map[string]string{
			"claude-mcp.json":   filepath.Join(home, ".claude.json"),
			"claude-hooks.json": filepath.Join(home, ".claude", "settings.json"),
			"codex-mcp.toml":    filepath.Join(home, ".codex", "config.toml"),
			"codex-hooks.json":  filepath.Join(home, ".codex", "hooks.json"),
			"cursor-hooks.json": filepath.Join(home, ".cursor", "hooks.json"),
			"cursor-mcp.json":   filepath.Join(home, ".cursor", "mcp.json"),
		}
		if !reflect.DeepEqual(targets, wantTargetsByDraft) {
			t.Errorf("targets.json = %v, want %v", targets, wantTargetsByDraft)
		}
		if !strings.HasSuffix(readFileString(t, filepath.Join(out, "targets.json")), "}\n") {
			t.Error("targets.json must end with a newline")
		}
	})

	t.Run("relocated destinations", func(t *testing.T) {
		isolateConfigEnv(t)
		home, custom := t.TempDir(), t.TempDir()
		t.Setenv("CODEX_HOME", filepath.Join(custom, "codex"))
		t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(custom, "claude"))
		t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(custom, "cursor")) // Declared files ignore this one.
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(custom, "xdg"))      // So does this one.
		out := filepath.Join(home, "drafts")
		if err := install.WriteSetup(configDeclare, binary, filepath.Join(home, "state"), out, home); err != nil {
			t.Fatal(err)
		}
		var targets map[string]string
		if err := json.Unmarshal([]byte(readFileString(t, filepath.Join(out, "targets.json"))), &targets); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			// The file's base name moves under the variable's directory.
			"claude-mcp.json":   filepath.Join(custom, "claude", ".claude.json"),
			"claude-hooks.json": filepath.Join(custom, "claude", "settings.json"),
			"codex-mcp.toml":    filepath.Join(custom, "codex", "config.toml"),
			"codex-hooks.json":  filepath.Join(custom, "codex", "hooks.json"),
			"cursor-hooks.json": filepath.Join(home, ".cursor", "hooks.json"),
			"cursor-mcp.json":   filepath.Join(home, ".cursor", "mcp.json"),
		}
		if !reflect.DeepEqual(targets, want) {
			t.Errorf("targets.json = %v\nwant %v", targets, want)
		}
	})

	t.Run("a blank variable means unset", func(t *testing.T) {
		isolateConfigEnv(t)
		home := t.TempDir()
		t.Setenv("CODEX_HOME", " \t ")
		out := filepath.Join(home, "drafts")
		if err := install.WriteSetup(configDeclare, binary, filepath.Join(home, "state"), out, home); err != nil {
			t.Fatal(err)
		}
		var targets map[string]string
		if err := json.Unmarshal([]byte(readFileString(t, filepath.Join(out, "targets.json"))), &targets); err != nil {
			t.Fatal(err)
		}
		if targets["codex-hooks.json"] != filepath.Join(home, ".codex", "hooks.json") {
			t.Errorf("a blank CODEX_HOME relocated the file to %q", targets["codex-hooks.json"])
		}
	})
}

func TestSetupRefusals(t *testing.T) {
	const binary = "/bin/zalki"
	tests := []struct {
		name string
		env  map[string]string
		home func(string) string
		want string
	}{
		{"relative CODEX_HOME", map[string]string{"CODEX_HOME": "relative/dir"}, func(h string) string { return h },
			"CODEX_HOME must be an absolute configuration directory"},
		{"relative CLAUDE_CONFIG_DIR", map[string]string{"CLAUDE_CONFIG_DIR": "."}, func(h string) string { return h },
			"CLAUDE_CONFIG_DIR must be an absolute configuration directory"},
		{"relative home", nil, func(string) string { return "relative-home" },
			"home path must be absolute"},
		{"two files on one target", map[string]string{"CODEX_HOME": "@home/.cursor"}, func(h string) string { return h },
			"configuration targets overlap each other or installation state"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateConfigEnv(t)
			base := t.TempDir()
			for name, value := range test.env {
				t.Setenv(name, strings.ReplaceAll(value, "@home", base))
			}
			out := filepath.Join(base, "drafts")
			err := install.WriteSetup(configDeclare, binary, filepath.Join(base, "state"), out, test.home(base))
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatalf("a refused setup created the draft directory: %v", statErr)
			}
		})
	}
}

func TestInstallRecordAndBackupContracts(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	const binary, newBinary = "/opt/my tools/zalki", "/opt/new tools/zalki"
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{"theme":"mine"}`
	if err := os.WriteFile(settings, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(state, "installation.json")
	backups := func(path string) []string {
		found, err := filepath.Glob(path + ".zalki-backup-*")
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(found)
		return found
	}
	request := func(action, bin string) install.Request {
		return install.Request{Action: action, Binary: bin, StateDir: state, HomeDir: home, Apply: true}
	}

	report, err := install.Manage(configDeclare, request("install", binary))
	targets := wantTargets(home)
	var wantPaths []string
	for _, target := range targets {
		wantPaths = append(wantPaths, target)
	}
	sort.Strings(wantPaths)
	if err != nil || report.Action != "install" || report.DryRun || !reflect.DeepEqual(report.Paths, wantPaths) {
		t.Fatalf("install report = %+v, %v; want the six targets in sorted order", report, err)
	}

	// The installation record.
	text := strings.ReplaceAll(readFileString(t, record), home, "<home>")
	if !strings.HasPrefix(text, "{\n  \"schema\": 3,\n  \"home\": \"<home>\",\n  \"binary\": \""+binary+"\",\n  \"state\": \"<home>/state\",\n  \"patches\": {") || !strings.HasSuffix(text, "}\n") {
		t.Errorf("installation.json layout changed; it starts with %q", text[:min(len(text), 160)])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(readFileString(t, record)), &got); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"binary", "fragments", "home", "patches", "schema", "state", "targets"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("installation.json keys = %v, want %v", keys, want)
	}
	if got["schema"] != float64(3) || got["home"] != home || got["binary"] != binary || got["state"] != state {
		t.Errorf("installation.json header = %v %v %v %v", got["schema"], got["home"], got["binary"], got["state"])
	}
	patches := got["patches"].(map[string]any)
	patchPaths := make([]string, 0, len(patches))
	for path := range patches {
		patchPaths = append(patchPaths, path)
	}
	sort.Strings(patchPaths)
	if want := []string{".claude.json", ".claude/settings.json", ".codex/hooks.json", ".cursor/hooks.json", ".cursor/mcp.json"}; !reflect.DeepEqual(patchPaths, want) {
		t.Errorf("recorded patches = %v, want %v", patchPaths, want)
	}
	wantMCP := map[string]any{"mcpServers": map[string]any{"zalki": map[string]any{"command": binary, "args": []any{"--state", state, "mcp"}, "type": "stdio"}}}
	if !reflect.DeepEqual(patches[".claude.json"], wantMCP) {
		t.Errorf("recorded .claude.json patch = %v, want %v", patches[".claude.json"], wantMCP)
	}
	wantFragments := map[string]any{".codex/config.toml": "\n[mcp_servers.zalki]\ncommand = \"" + binary + "\"\nargs = [\"--state\",\"" + state + "\",\"mcp\"]\n"}
	if !reflect.DeepEqual(got["fragments"], wantFragments) {
		t.Errorf("recorded fragments = %v, want %v", got["fragments"], wantFragments)
	}
	gotTargets := map[string]string{}
	for key, value := range got["targets"].(map[string]any) {
		gotTargets[key] = value.(string)
	}
	if !reflect.DeepEqual(gotTargets, targets) {
		t.Errorf("recorded targets = %v, want %v", gotTargets, targets)
	}

	// New files and records are private; the existing fixture is private too.
	// Preservation of other pre-existing modes is a separate policy question.
	for _, path := range append([]string{record}, wantPaths...) {
		if mode := modeOf(t, path); mode != 0600 {
			t.Errorf("%s: mode %v after install, want 0600", strings.TrimPrefix(path, home), mode)
		}
	}

	// Backups: only the file that existed, holding its original bytes.
	found := backups(settings)
	if len(found) != 1 || readFileString(t, found[0]) != original || modeOf(t, found[0]) != 0600 {
		t.Fatalf("settings backups = %v", found)
	}
	for _, path := range wantPaths {
		if path != settings && len(backups(path)) != 0 {
			t.Errorf("%s: a new file got a backup", strings.TrimPrefix(path, home))
		}
	}
	if len(backups(record)) != 0 {
		t.Error("the first record got a backup")
	}

	// Upgrading to a new binary rewrites every file once and backs each one up
	// with the bytes it held, under one shared stamp.
	priorBytes := map[string]string{record: readFileString(t, record)}
	for _, path := range wantPaths {
		priorBytes[path] = readFileString(t, path)
	}
	knownBackups := map[string]bool{}
	for path := range priorBytes {
		for _, existing := range backups(path) {
			knownBackups[existing] = true
		}
	}
	report, err = install.Manage(configDeclare, request("upgrade", newBinary))
	if err != nil || !reflect.DeepEqual(report.Paths, wantPaths) {
		t.Fatalf("upgrade report = %+v, %v", report, err)
	}
	stamps := map[string]bool{}
	for path, prior := range priorBytes {
		var added []string
		for _, candidate := range backups(path) {
			if !knownBackups[candidate] {
				added = append(added, candidate)
			}
		}
		if len(added) != 1 {
			t.Errorf("%s: upgrade added %d backups, want 1", strings.TrimPrefix(path, home), len(added))
			continue
		}
		newest := added[0]
		if readFileString(t, newest) != prior {
			t.Errorf("%s: the newest backup is not the pre-upgrade file", strings.TrimPrefix(path, home))
		}
		stamps[strings.TrimPrefix(newest, path)] = true
	}
	if len(stamps) != 1 {
		t.Errorf("one upgrade used several backup stamps: %v", stamps)
	}
	if body := readFileString(t, record); !strings.Contains(body, `"binary": "`+newBinary+`"`) || strings.Contains(body, `"binary": "`+binary+`"`) {
		t.Error("the record does not name the new binary")
	}
	if strings.Contains(readFileString(t, filepath.Join(home, ".cursor", "mcp.json")), binary) {
		t.Error("the old binary is still configured for Cursor")
	}

}

func TestPreviewAndPlanNeverChangeTheDeclaredMaps(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	cached := map[string][]install.ConfigFile{}
	pristine := map[string]string{}
	snapshotMaps := func(binary string) string {
		var all []any
		for _, file := range cached[binary] {
			all = append(all, file.JSON)
		}
		b, err := json.Marshal(all)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// Return shared maps, but capture their baseline before the caller can
	// touch them. This also covers any validation/template declarations without
	// depending on the installer's internal choice of template binary.
	declare := func(binary, stateDir string) ([]install.ConfigFile, error) {
		if files, ok := cached[binary]; ok {
			return files, nil
		}
		files, err := configDeclare(binary, stateDir)
		if err != nil {
			return nil, err
		}
		cached[binary] = files
		pristine[binary] = snapshotMaps(binary)
		return files, nil
	}
	steps := []struct {
		name   string
		action string
		binary string
		apply  bool
	}{
		{"install preview", "install", "/bin/one", false},
		{"install", "install", "/bin/one", true},
		{"repeat install", "install", "/bin/one", true},
		{"upgrade preview", "upgrade", "/bin/two", false},
		{"upgrade", "upgrade", "/bin/two", true},
		{"uninstall preview", "uninstall", "/bin/two", false},
		{"uninstall", "uninstall", "/bin/two", true},
	}
	checkMaps := func(step string) {
		t.Helper()
		for binary, before := range pristine {
			if now := snapshotMaps(binary); now != before {
				t.Errorf("%s changed the declared JSON maps for %s\nbefore %s\n after %s", step, binary, before, now)
			}
		}
	}
	for _, step := range steps {
		if _, err := install.Manage(declare, install.Request{Action: step.action, Binary: step.binary, StateDir: state, HomeDir: home, Apply: step.apply}); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		checkMaps(step.name)
	}
	if err := install.WriteSetup(declare, "/bin/two", state, filepath.Join(home, "drafts"), home); err != nil {
		t.Fatal(err)
	}
	checkMaps("setup")
}

func TestDeclarationsAreReadOnEveryCallAndNeverCached(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	patch := map[string]any{"mcpServers": map[string]any{"zalki": map[string]any{"command": "/bin/one"}}}
	declare := func(string, string) ([]install.ConfigFile, error) {
		return []install.ConfigFile{{Path: ".solo/mcp.json", Draft: "solo.json", JSON: patch}}, nil
	}
	request := func(action string, apply bool) install.Request {
		return install.Request{Action: action, Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: apply}
	}
	if _, err := install.Manage(declare, request("install", true)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".solo", "mcp.json")
	if !strings.Contains(readFileString(t, target), `"/bin/one"`) {
		t.Fatal("first install did not write the declared command")
	}
	// The caller changes the very map it handed over. The record on disk is
	// the snapshot of what was installed, so the next upgrade replaces the old
	// entry with the new one.
	patch["mcpServers"].(map[string]any)["zalki"].(map[string]any)["command"] = "/bin/two"
	preview, err := install.Manage(declare, request("upgrade", false))
	if err != nil || len(preview.Paths) != 1 {
		t.Fatalf("preview after mutating the declared map: %+v, %v", preview, err)
	}
	if _, err := install.Manage(declare, request("upgrade", true)); err != nil {
		t.Fatal(err)
	}
	if body := readFileString(t, target); !strings.Contains(body, `"/bin/two"`) || strings.Contains(body, `"/bin/one"`) {
		t.Fatalf("upgrade did not replace the entry the record owned: %s", body)
	}
	if bytes.Contains([]byte(readFileString(t, filepath.Join(state, "installation.json"))), []byte(`"/bin/one"`)) {
		t.Fatal("the record still names the old command")
	}
}
