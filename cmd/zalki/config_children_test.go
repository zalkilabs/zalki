package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/zalkilabs/zalki/internal/install"
)

func defaultConfigLine(home string) string {
	return "Config file: " + filepath.Join(home, ".config", "zalki", "config.json") + "\n"
}

type configChildSelection struct {
	home, configFile, state string
	selection               []string
}

func TestConfigChildrenPinSelectionInEveryClient(t *testing.T) {
	for _, source := range []string{"flag", "env", "default"} {
		t.Run(source, func(t *testing.T) { checkConfigChildSelection(t, source) })
	}
}

func newConfigChildSelection(t *testing.T, source string) configChildSelection {
	t.Helper()
	home := configTestHome(t)
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(name, "")
	}
	t.Chdir(home)
	configRel, stateRel := "settings ' $literal `printf altered`/config.json", "state ' $literal $(printf altered)"
	fixture := configChildSelection{home: home, configFile: filepath.Join(home, configRel), state: filepath.Join(home, stateRel)}
	switch source {
	case "flag":
		t.Setenv("ZALKI_CONFIG", filepath.Join(home, "unused-config.json"))
		t.Setenv("ZALKI_STATE", filepath.Join(home, "unused-state"))
		fixture.selection = []string{"--config", configRel, "--state", stateRel}
	case "env":
		t.Setenv("ZALKI_CONFIG", configRel)
		t.Setenv("ZALKI_STATE", stateRel)
	default:
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
		t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg-state"))
		fixture.configFile = filepath.Join(home, "xdg-config", "zalki", "config.json")
		fixture.state = filepath.Join(home, "xdg-state", "zalki")
	}
	writeConfigFixture(t, fixture.configFile, `{"logging":{"level":"off"}}`)
	return fixture
}

func (fixture configChildSelection) run(t *testing.T, args ...string) string {
	t.Helper()
	argv := append(append([]string{}, fixture.selection...), args...)
	out, diagnostic, err := execute(t, fixture.home, "", argv...)
	if err != nil || diagnostic != "" {
		t.Fatalf("%v: err=%v diagnostic=%q", args, err, diagnostic)
	}
	return out
}

func checkConfigChildSelection(t *testing.T, source string) {
	t.Helper()
	fixture := newConfigChildSelection(t, source)
	configBefore, _ := os.ReadFile(fixture.configFile)
	checkConfigChildDrafts(t, fixture)
	checkConfigChildInstall(t, fixture)
	checkConfigChildUpgrade(t, fixture)
	fixture.run(t, "uninstall", "--apply")
	configAfter, err := os.ReadFile(fixture.configFile)
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		t.Fatal("client configuration operation changed the selected settings file", err)
	}
}

func checkConfigChildDrafts(t *testing.T, fixture configChildSelection) {
	t.Helper()
	wantLine := "Config file: " + fixture.configFile + "\n"
	for _, action := range []string{"setup", "install"} {
		drafts := filepath.Join(fixture.home, "drafts-"+action)
		out := fixture.run(t, action, "--out", drafts)
		if !strings.HasPrefix(out, wantLine) || strings.Count(out, "Config file:") != 1 {
			t.Fatalf("%s review must show selected config once: %q", action, out)
		}
		for _, name := range []string{"claude-hooks.json", "codex-hooks.json", "cursor-hooks.json", "claude-mcp.json", "codex-mcp.toml", "cursor-mcp.json"} {
			checkChildConfiguration(t, filepath.Join(drafts, name), fixture.configFile, fixture.state)
		}
	}
}

func checkConfigChildInstall(t *testing.T, fixture configChildSelection) {
	t.Helper()
	wantLine := "Config file: " + fixture.configFile + "\n"
	if out := fixture.run(t, "install", "--apply"); strings.Count(out, wantLine) != 1 {
		t.Fatalf("install selected config line: %q", out)
	}
	for _, path := range configTargets(fixture.home) {
		checkChildConfiguration(t, path, fixture.configFile, fixture.state)
	}
	before := cliSnapshot(t, fixture.home)
	if out := fixture.run(t, "install", "--apply"); out != wantLine+installAppliedNote || !reflect.DeepEqual(before, cliSnapshot(t, fixture.home)) {
		t.Fatal("repeat install changed settled configuration or output")
	}
}

func checkConfigChildUpgrade(t *testing.T, fixture configChildSelection) {
	t.Helper()
	before := cliSnapshot(t, fixture.home)
	binary, err := installationBinary()
	if err != nil {
		t.Fatal(err)
	}
	old := locateInstallation
	locateInstallation = func() (binaryLocation, error) { return binaryLocation{Binary: binary, Resolved: binary}, nil }
	t.Cleanup(func() { locateInstallation = old })
	preview := fixture.run(t, "upgrade", "--dry-run")
	if !json.Valid([]byte(preview)) || strings.Contains(preview, "Config file:") || !reflect.DeepEqual(before, cliSnapshot(t, fixture.home)) {
		t.Fatal("upgrade preview changed its JSON output or files")
	}
	if out := fixture.run(t, "upgrade", "--config-only"); out != configOnlyDone+"\n" || !reflect.DeepEqual(before, cliSnapshot(t, fixture.home)) {
		t.Fatal("configuration refresh did not retain pinned selection")
	}
}

// Check literal argv after decoding each client's real JSON/TOML format. Shell
// commands are parsed with set, so the declared executable is never invoked.
func checkChildConfiguration(t *testing.T, path, configFile, state string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--config", configFile, "--state", state}
	if strings.HasSuffix(path, ".toml") {
		var value struct {
			Servers map[string]struct{ Args []string } `toml:"mcp_servers"`
		}
		if err := toml.Unmarshal(data, &value); err != nil {
			t.Fatal(err)
		}
		checkChildMCPArgs(t, path, value.Servers["zalki"].Args, want)
		return
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if servers, ok := value["mcpServers"].(map[string]any); ok {
		server := servers["zalki"].(map[string]any)
		var args []string
		for _, arg := range server["args"].([]any) {
			args = append(args, arg.(string))
		}
		checkChildMCPArgs(t, path, args, want)
		return
	}
	commands := childHookCommands(value)
	for _, command := range commands {
		checkChildHookArgs(t, path, command, want)
	}
	if len(commands) == 0 {
		t.Fatalf("%s: no hook commands", path)
	}
}

func checkChildMCPArgs(t *testing.T, path string, args, want []string) {
	t.Helper()
	if !reflect.DeepEqual(args, append(append([]string{}, want...), "mcp")) {
		t.Fatalf("%s: MCP argv=%q", path, args)
	}
}

func childHookCommands(value any) []string {
	var commands []string
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			commands = append(commands, childHookCommands(child)...)
		}
	case map[string]any:
		for key, child := range value {
			if key == "command" {
				commands = append(commands, child.(string))
			} else {
				commands = append(commands, childHookCommands(child)...)
			}
		}
	}
	return commands
}

func checkChildHookArgs(t *testing.T, path, command string, want []string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", "set -- "+command+"\nprintf '%s\\000' \"$@\"").Output()
	args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if err != nil || len(args) != 8 || !reflect.DeepEqual(args[1:5], want) || args[5] != "hook" || args[6] != "--agent" {
		t.Fatalf("%s: hook argv=%q err=%v", path, args, err)
	}
}

func TestConfigChildrenUpgradeHandoffPinsAbsolutePaths(t *testing.T) {
	for _, source := range []string{"flag", "env"} {
		t.Run(source, func(t *testing.T) {
			f := newUpgradeFixture(t, "", "")
			t.Chdir(f.home)
			configRel := "settings ' $literal `printf altered`.json"
			configFile := filepath.Join(f.home, configRel)
			writeConfigFixture(t, configFile, `{"logging":{"level":"off"}}`)
			t.Setenv("ZALKI_CONFIG", configRel)
			t.Setenv("ZALKI_STATE", "state")
			t.Setenv("ZALKI_TEST_K3_ARGS", f.childLog)
			cask := filepath.Join(f.prefix, "Caskroom", "zalki", "2.0.0", "zalki")
			if err := os.WriteFile(cask, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\" > \"$ZALKI_TEST_K3_ARGS\"\nprintf '%s\\n' 'Upgrade successful'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"upgrade", "--log-level=off", "--log-file=-"}
			if source == "flag" {
				t.Setenv("ZALKI_CONFIG", filepath.Join(f.home, "unused.json"))
				t.Setenv("ZALKI_STATE", filepath.Join(f.home, "unused-state"))
				args = append(args, "--config", configRel, "--state", "state")
			}
			before := f.manifestBytes(t)
			out, diagnostic, err := execute(t, f.home, "", args...)
			if err != nil || diagnostic != "" || out != upgradeSuccess+"\n" || f.manifestBytes(t) != before {
				t.Fatal("old build handoff changed output or configuration", err)
			}
			data, err := os.ReadFile(f.childLog)
			got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			want := []string{"upgrade", "--finish-upgrade", "--config", configFile, "--state", f.state, "--log-level=off", "--log-file=-"}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("new binary argv=%q err=%v; want %q", got, err, want)
			}
		})
	}
}

func TestConfigChildrenCursorStartupAndRecovery(t *testing.T) {
	home := configTestHome(t)
	configFile := filepath.Join(home, "settings ' $literal.json")
	state := filepath.Join(home, "selected-state")
	writeConfigFixture(t, configFile, `{"logging":{"level":"off"}}`)
	t.Setenv("ZALKI_CONFIG", configFile)
	t.Setenv("ZALKI_STATE", state)
	const native = "61000000-0000-4000-8000-000000000001"
	for _, event := range []string{"sessionStart", "postToolUse"} {
		input, _ := json.Marshal(map[string]any{"conversation_id": native, "hook_event_name": event, "cwd": home})
		out, diagnostic, err := execute(t, home, string(input), "hook", "--agent", "cursor")
		var value struct {
			Context string `json:"additional_context"`
		}
		if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &value) != nil {
			t.Fatal("Cursor hook failed", event, err)
		}
		selection := install.ShellQuote("--config") + " " + install.ShellQuote(configFile) + " " + install.ShellQuote("--state") + " " + install.ShellQuote(state)
		if !strings.Contains(value.Context, selection) || strings.Count(value.Context, install.ShellQuote("--config")) != 1 {
			t.Fatal("Cursor hook guidance did not pin the selected config/state", event)
		}
	}
}

func TestConfigChildrenRealUpgradeSuccessOutput(t *testing.T) {
	binary := checkRealBinary(t)
	home := configTestHome(t)
	t.Setenv("HOME", home)
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, "")
	}
	configFile, state := filepath.Join(home, "selected.json"), filepath.Join(home, "state")
	writeConfigFixture(t, configFile, `{"logging":{"level":"off"}}`)
	if _, _, err := execute(t, home, "", "--config", configFile, "--state", state, "install", "--apply"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct{ flag, want string }{{"--finish-upgrade", upgradeSuccess}, {"--config-only", configOnlyDone}} {
		if out, diagnostic, err := execute(t, home, "", "--config", configFile, "--state", state, "upgrade", mode.flag); err != nil || diagnostic != "" || out != mode.want+"\n" {
			t.Fatalf("in-process %s output=%q diagnostic=%q err=%v", mode.flag, out, diagnostic, err)
		}
		cmd := exec.CommandContext(t.Context(), binary, "--config", configFile, "--state", state, "upgrade", mode.flag)
		var out, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diagnostic
		if err := cmd.Run(); err != nil || diagnostic.Len() != 0 || out.String() != mode.want+"\n" {
			t.Fatalf("real %s output=%q diagnostic=%q err=%v", mode.flag, out.String(), diagnostic.String(), err)
		}
	}
}

type configChildClient struct{ name, target, variable string }

func TestConfigChildrenRejectsBootstrapTargetCollision(t *testing.T) {
	for _, client := range []configChildClient{{"claude", ".claude.json", "CLAUDE_CONFIG_DIR"}, {"codex", ".codex/hooks.json", "CODEX_HOME"}, {"cursor", ".cursor/mcp.json", ""}} {
		for _, alias := range []string{"direct", "hardlink", "parent-symlink", "config-symlink", "relocated"} {
			if alias == "relocated" && client.variable == "" {
				continue
			}
			for _, action := range []string{"install", "setup", "upgrade"} {
				t.Run(client.name+"/"+alias+"/"+action, func(t *testing.T) { checkBootstrapTargetCollision(t, client, alias, action) })
			}
		}
	}
}

func prepareBootstrapCollision(t *testing.T, client configChildClient, alias, action string) (home, state, selected string) {
	t.Helper()
	home = configTestHome(t)
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(variable, "")
	}
	state = filepath.Join(home, "state")
	target := filepath.Join(home, client.target)
	if alias == "relocated" {
		root := filepath.Join(home, "relocated")
		t.Setenv(client.variable, root)
		target = filepath.Join(root, filepath.Base(client.target))
	}
	if action == "upgrade" {
		prepareBootstrapUpgrade(t, home, state)
	}
	writeConfigFixture(t, target, `{}`)
	return home, state, bootstrapConfigAlias(t, alias, home, target)
}

func prepareBootstrapUpgrade(t *testing.T, home, state string) {
	t.Helper()
	safe := filepath.Join(home, "zalki.json")
	writeConfigFixture(t, safe, `{}`)
	if _, _, err := execute(t, home, "", "--config", safe, "--state", state, "--log-level=off", "install", "--apply"); err != nil {
		t.Fatal(err)
	}
}

func bootstrapConfigAlias(t *testing.T, alias, home, target string) string {
	t.Helper()
	selected := target
	var err error
	switch alias {
	case "hardlink":
		selected = filepath.Join(home, "config-alias.json")
		err = os.Link(target, selected)
	case "parent-symlink":
		link := filepath.Join(home, "parent-alias")
		err = os.Symlink(filepath.Dir(target), link)
		selected = filepath.Join(link, filepath.Base(target))
	case "config-symlink":
		selected = filepath.Join(home, "config-alias.json")
		err = os.Symlink(target, selected)
	}
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

func checkBootstrapTargetCollision(t *testing.T, client configChildClient, alias, action string) {
	t.Helper()
	home, state, selected := prepareBootstrapCollision(t, client, alias, action)
	args := []string{"--config", selected, "--state", state, "--log-level=off", action}
	switch action {
	case "install":
		args = append(args, "--apply")
	case "setup":
		args = append(args, "--out", filepath.Join(home, "drafts"))
	case "upgrade":
		args = append(args, "--config-only")
	}
	before := bootstrapSnapshot(t, home)
	out, diagnostic, err := execute(t, home, "", args...)
	if err == nil || out != "" || diagnostic != "" || strings.Contains(err.Error(), "\n") {
		t.Fatal("bootstrap collision did not fail cleanly", err)
	}
	if alias != "config-symlink" && (!strings.Contains(err.Error(), "--config points at a file Zalki edits for") || !strings.Contains(err.Error(), "choose another config file")) {
		t.Fatal("bootstrap collision lacked the selection repair", err)
	}
	if !reflect.DeepEqual(before, bootstrapSnapshot(t, home)) {
		t.Fatal("installer wrote client/config files or backups after refusing the selection")
	}
}

func TestConfigChildrenClientRootDiagnostics(t *testing.T) {
	for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		for name, control := range map[string]string{"newline": "\n", "escape": "\x1b"} {
			t.Run(variable+"/"+name, func(t *testing.T) {
				home := configTestHome(t)
				t.Setenv(variable, filepath.Join(home, "bad"+control+"root"))
				out, diagnostic, err := execute(t, home, "", "--log-level=debug", "--log-file=-", "version")
				if err == nil || out != "" || diagnostic != "" || strings.Contains(err.Error(), control) || !strings.Contains(err.Error(), variable+": invalid path") {
					t.Fatal("client root entered diagnostic output before validation", err)
				}
			})
		}
	}
}

func TestConfigChildrenLogFileCannotOverwriteClientSettings(t *testing.T) {
	for _, client := range []struct{ name, target, variable string }{
		{"claude", ".claude.json", "CLAUDE_CONFIG_DIR"},
		{"codex", ".codex/hooks.json", "CODEX_HOME"},
		{"cursor", ".cursor/mcp.json", ""},
	} {
		for _, relocated := range []bool{false, true} {
			if relocated && client.variable == "" {
				continue
			}
			t.Run(client.name+map[bool]string{true: "/relocated", false: "/default"}[relocated], func(t *testing.T) {
				home := configTestHome(t)
				for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
					t.Setenv(variable, "")
				}
				configFile := filepath.Join(home, "zalki.json")
				writeConfigFixture(t, configFile, `{}`)
				target := filepath.Join(home, client.target)
				if relocated {
					root := filepath.Join(home, "relocated")
					t.Setenv(client.variable, root)
					target = filepath.Join(root, filepath.Base(client.target))
				}
				writeConfigFixture(t, target, `{}`)
				before := bootstrapSnapshot(t, home)
				out, diagnostic, err := execute(t, home, "", "--config", configFile, "--log-level=debug", "--log-file", target, "version")
				if err == nil || out != "" || diagnostic != "" || !strings.Contains(err.Error(), "--log-file points at a file Zalki edits") || !strings.Contains(err.Error(), "choose another log file") || !reflect.DeepEqual(before, bootstrapSnapshot(t, home)) {
					t.Fatal("diagnostic sink could overwrite managed client settings", err)
				}
			})
		}
	}
}

func bootstrapSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += link
		} else if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += string(data)
		}
		files[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
