package install

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

// ShellQuote single-quotes value for safe use in a POSIX shell command.
func ShellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// WriteSetup writes reviewable configuration drafts for the declared files.
func WriteSetup(declare Declare, binary, state, out, home, configFile string) error {
	files, err := declare(binary, state)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(home) {
		return errors.New("home path must be absolute")
	}
	targets, err := setupTargets(files, Request{StateDir: state, HomeDir: home, ConfigFile: configFile})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		return err
	}
	for _, file := range files {
		var data []byte
		if file.JSON != nil {
			data, err = json.MarshalIndent(file.JSON, "", "  ")
			if err != nil {
				return err
			}
			data = append(data, '\n')
		} else {
			data = []byte(file.TOML)
		}
		if err := fsutil.WriteExclusive(filepath.Join(out, file.Draft), data); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(targets, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteExclusive(filepath.Join(out, "targets.json"), append(data, '\n'))
}

// mergeConfig adds the entries of patch to a JSON settings file. It refuses
// to change entries the user already has in a different form.
func mergeConfig(existing []byte, patch map[string]any) ([]byte, error) {
	current, err := decodeSettingsToMerge(existing)
	if err != nil {
		return nil, err
	}
	for key, value := range patch {
		if err := mergePatchKey(current, key, value); err != nil {
			return nil, err
		}
	}
	return encodeSettings(current)
}

// decodeSettingsToMerge reads a settings file. An empty file counts as an
// empty object.
func decodeSettingsToMerge(existing []byte) (map[string]any, error) {
	current := map[string]any{}
	if len(existing) == 0 {
		return current, nil
	}
	if err := json.Unmarshal(existing, &current); err != nil {
		return nil, err
	}
	if current == nil {
		return nil, errors.New("settings must be a JSON object")
	}
	return current, nil
}

// mergePatchKey merges what one top-level key of a patch adds.
func mergePatchKey(current map[string]any, key string, value any) error {
	switch key {
	case "version":
		return mergeSchemaVersion(current, value)
	case "mcpServers":
		return mergeMCPServer(current, value)
	case "hooks":
		return mergeHooks(current, value)
	default:
		return errors.New("unexpected settings key")
	}
}

// mergeSchemaVersion sets the Cursor hook schema version. It refuses another
// version that is already there.
func mergeSchemaVersion(current map[string]any, version any) error {
	if old, exists := current["version"]; exists && old != float64(1) {
		return errors.New("unsupported existing Cursor hook schema version")
	}
	current["version"] = version
	return nil
}

// mergeMCPServer adds the zalki MCP server. It refuses a different server
// with the same name.
func mergeMCPServer(current map[string]any, value any) error {
	servers, ok := current["mcpServers"].(map[string]any)
	if current["mcpServers"] != nil && !ok {
		return errors.New("existing mcpServers is not an object")
	}
	if servers == nil {
		servers = map[string]any{}
	}
	wanted := value.(map[string]any)[zalkiMCPServer]
	if old, exists := servers[zalkiMCPServer]; exists && !equalJSON(old, wanted) {
		return errors.New("an existing zalki MCP server differs; review it before replacing")
	}
	servers[zalkiMCPServer] = wanted
	current["mcpServers"] = servers
	return nil
}

// mergeHooks adds the wanted hook of each event next to the hooks the user has.
func mergeHooks(current map[string]any, value any) error {
	hooks, ok := current["hooks"].(map[string]any)
	if current["hooks"] != nil && !ok {
		return errors.New("existing hooks is not an object")
	}
	if hooks == nil {
		hooks = map[string]any{}
	}
	for event, addition := range value.(map[string]any) {
		if err := mergeHookEvent(hooks, event, addition); err != nil {
			return err
		}
	}
	current["hooks"] = hooks
	return nil
}

// mergeHookEvent appends the wanted entry to one hook event unless it is there.
func mergeHookEvent(hooks map[string]any, event string, addition any) error {
	entries, ok := hooks[event].([]any)
	if hooks[event] != nil && !ok {
		return errors.New("existing hook event is not an array")
	}
	wanted := addition.([]any)[0]
	if !containsJSON(entries, wanted) {
		entries = append(entries, wanted)
	}
	hooks[event] = entries
	return nil
}

type configEdit struct {
	path          string
	before, after []byte
	existed       bool
	backup        string
	remove        bool
}

// Apply installs the reviewed user-config additions and records their ownership.
func Apply(declare Declare, binary, state, home string) ([]string, error) {
	report, err := Manage(declare, Request{Action: "install", Binary: binary, StateDir: state, HomeDir: home, Apply: true})
	return report.Paths, err
}

func configBytes(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
		return nil, false, errors.New("refusing non-regular or oversized settings file")
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is a client settings file the installer owns
	return b, true, err
}

func setupTargets(files []ConfigFile, request Request) (map[string]string, error) {
	targets := map[string]string{}
	seen := map[string]bool{}
	for _, file := range files {
		target, err := file.target(request.HomeDir)
		if err != nil {
			return nil, err
		}
		if err := validateReservedTarget(target, file, request); err != nil {
			return nil, err
		}
		if seen[target] || target == filepath.Join(request.StateDir, "installation.json") {
			return nil, errors.New("configuration targets overlap each other or installation state")
		}
		seen[target] = true
		targets[file.Draft] = target
	}
	return targets, nil
}
