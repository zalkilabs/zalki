package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

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
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty; choose an empty folder for the drafts", out)
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
// to change entries the user already has in a different form. owned holds the
// recorded patches of an upgrade: the zalki MCP entry is then updated key by key
// and the user's own keys in it are kept.
func mergeConfig(existing []byte, patch map[string]any, owned []map[string]any, action string) ([]byte, error) {
	current, err := decodeSettingsToMerge(existing)
	if err != nil {
		return nil, err
	}
	for key, value := range patch {
		if err := mergePatchKey(current, key, value, owned, action); err != nil {
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
	if err := decodeSettings(existing, &current); err != nil {
		return nil, err
	}
	if current == nil {
		return nil, errors.New("settings must be a JSON object")
	}
	return current, nil
}

// mergePatchKey merges what one top-level key of a patch adds.
func mergePatchKey(current map[string]any, key string, value any, owned []map[string]any, action string) error {
	switch key {
	case "version":
		return mergeSchemaVersion(current, value)
	case "mcpServers":
		return mergeMCPServer(current, value, ownedEntries(owned))
	case "hooks":
		return mergeHooks(current, value, owned, action)
	default:
		return errors.New("unexpected settings key")
	}
}

// mergeSchemaVersion sets the Cursor hook schema version. It refuses another
// version that is already there.
func mergeSchemaVersion(current map[string]any, version any) error {
	old, exists := current["version"]
	if exists && !equalJSON(old, 1) {
		return errors.New("unsupported existing Cursor hook schema version")
	}
	if !exists {
		current["version"] = version
	}
	return nil
}

// mergeMCPServer adds the zalki MCP server. An existing entry is accepted when
// it already holds every owned key with the next value, and is updated key by key
// when it still holds a recorded entry. It refuses an entry that differs in an owned key.
func mergeMCPServer(current map[string]any, value any, versions []map[string]any) error {
	servers, ok := current["mcpServers"].(map[string]any)
	if current["mcpServers"] != nil && !ok {
		return errors.New("existing mcpServers is not an object")
	}
	if servers == nil {
		servers = map[string]any{}
	}
	if err := mergeOwnedInto(servers, zalkiMCPServer, value.(map[string]any)[zalkiMCPServer], versions, mcpEntryWording); err != nil {
		return err
	}
	current["mcpServers"] = servers
	return nil
}

// mergeOwnedInto sets container[name] to the wanted entry. An existing entry is
// merged: it is accepted when it holds every owned key with the next value, and
// updated key by key when it still holds a recorded entry.
func mergeOwnedInto(container map[string]any, name string, wanted any, versions []map[string]any, w entryWording) error {
	if old, exists := container[name]; exists {
		merged, err := mergeOwnedEntry(old, wanted, versions, w)
		if err != nil {
			return err
		}
		wanted = merged
	}
	container[name] = wanted
	return nil
}

// mergeHooks adds the wanted hook of each event next to the hooks the user has.
// owned holds the recorded patches of an upgrade: a hook handler with a recorded
// command is zalki's, and gets the new command instead of a second hook.
func mergeHooks(current map[string]any, value any, owned []map[string]any, action string) error {
	hooks, ok := current["hooks"].(map[string]any)
	if current["hooks"] != nil && !ok {
		return errors.New("existing hooks is not an object")
	}
	if hooks == nil {
		hooks = map[string]any{}
	}
	for event, addition := range value.(map[string]any) {
		if err := mergeHookEvent(hooks, event, addition, recordedHooks(owned, event), action); err != nil {
			return err
		}
	}
	current["hooks"] = hooks
	return nil
}

// mergeHookEvent adds the wanted entry to one hook event unless it is there.
func mergeHookEvent(hooks map[string]any, event string, addition any, recorded map[string]recordedHook, action string) error {
	entries, ok := hooks[event].([]any)
	if hooks[event] != nil && !ok {
		return errors.New("existing hook event is not an array")
	}
	wanted := addition.([]any)[0]
	if !containsJSON(entries, wanted) {
		var err error
		if entries, err = addWantedHook(entries, event, wanted, recorded, action); err != nil {
			return err
		}
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
	// client is the kind of the client the file belongs to, or "".
	client string
	// notes are the owner's sentences about this edit, for a preview and a result.
	notes []editNote
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
