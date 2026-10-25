package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/zalkilabs/zalki/internal/fsutil"
)

type installation struct {
	Schema    int                       `json:"schema"`
	Home      string                    `json:"home"`
	Binary    string                    `json:"binary"`
	State     string                    `json:"state"`
	Patches   map[string]map[string]any `json:"patches"`
	Fragments map[string]string         `json:"fragments,omitempty"`
	Targets   map[string]string         `json:"targets,omitempty"`
}

type Request struct {
	Action   string
	Binary   string
	StateDir string
	HomeDir  string

	Apply bool
}

type Report struct {
	Action string   `json:"action"`
	DryRun bool     `json:"dry_run"`
	Paths  []string `json:"paths"`
}

// Declare returns the configuration files the compiled-in adapters own for a binary and state directory.
type Declare func(binary, state string) ([]ConfigFile, error)

// Manage plans every edit before writing and preserves exact ownership.
func Manage(declare Declare, request Request) (Report, error) {
	preview := request
	preview.Apply = false
	plan, err := declare.plan(preview)
	if err != nil || !request.Apply {
		return plan.report, err
	}
	if request.Action == "uninstall" {
		_, exists, err := configBytes(filepath.Join(request.StateDir, "installation.json"))
		if err != nil {
			return plan.report, err
		}
		if !exists {
			plan.report.DryRun = false
			return plan.report, nil
		}
	}
	unlock, err := lockInstallation(request.HomeDir)
	if err != nil {
		return plan.report, err
	}
	defer unlock()
	// Rebuild the complete plan under the home-wide lock before any write.
	plan, err = declare.plan(request)
	if err != nil {
		return plan.report, err
	}
	return plan.report, applyConfigEdits(plan.edits)
}

func (record installation) validateTargets(declarations map[string]ConfigFile, manifest string) error {
	if len(record.Targets) != len(record.Patches)+len(record.Fragments) {
		return errors.New("installation record has missing or unexpected configuration targets")
	}
	seen := map[string]bool{}
	for key, target := range record.Targets {
		file, ok := declarations[key]
		if !ok || (record.Patches[key] == nil && record.Fragments[key] == "") {
			return errors.New("installation record has unexpected configuration targets")
		}
		if !filepath.IsAbs(target) || filepath.Clean(target) != target || filepath.Base(target) != filepath.Base(file.Path) {
			return errors.New("installation record has an invalid configuration target")
		}
		if file.ConfigRootEnv == "" && target != filepath.Join(record.Home, file.Path) {
			return errors.New("adapter does not support relocating this configuration file")
		}
		if seen[target] || target == manifest {
			return errors.New("configuration targets overlap each other or installation state")
		}
		seen[target] = true
	}
	return nil
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func validateInstallationPatch(patch map[string]any) error {
	if len(patch) == 0 {
		return errors.New("empty installation patch")
	}
	for key, value := range patch {
		switch key {
		case "version":
			if value != float64(1) {
				return errors.New("unsupported recorded hook version")
			}
		case "mcpServers":
			servers, ok := value.(map[string]any)
			if !ok {
				return errors.New("invalid recorded MCP entry")
			}
			if _, err := managedMCPName(servers); err != nil {
				return err
			}
		case "hooks":
			hooks, ok := value.(map[string]any)
			if !ok {
				return errors.New("invalid recorded hooks")
			}
			for _, value := range hooks {
				if entries, ok := value.([]any); !ok || len(entries) == 0 {
					return errors.New("invalid recorded hook entries")
				}
			}
		default:
			return errors.New("unexpected recorded settings key")
		}
	}
	return nil
}

func managedMCPName(servers map[string]any) (string, error) {
	if len(servers) == 1 {
		if _, ok := servers["zalki"].(map[string]any); ok {
			return "zalki", nil
		}
	}
	return "", errors.New("invalid recorded MCP entry; expected exactly one zalki server")
}

func removeConfig(before []byte, patch map[string]any) ([]byte, error) {
	var current map[string]any
	if err := json.Unmarshal(before, &current); err != nil || current == nil {
		return nil, errors.New("settings must be a JSON object")
	}
	for key, value := range patch {
		switch key {
		case "version": // Cursor schema version is shared with unrelated hooks.
		case "mcpServers":
			owned, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("invalid recorded MCP entry")
			}
			name, err := managedMCPName(owned)
			if err != nil {
				return nil, err
			}
			servers, ok := current[key].(map[string]any)
			if current[key] == nil {
				continue
			}
			if !ok {
				return nil, errors.New("mcpServers is not an object")
			}
			if found, exists := servers[name]; exists {
				if !equalJSON(found, owned[name]) {
					return nil, fmt.Errorf("managed %s MCP entry was edited; review it before removing or upgrading", name)
				}
				delete(servers, name)
			}
		case "hooks":
			hooks, ok := current[key].(map[string]any)
			if current[key] == nil {
				continue
			}
			if !ok {
				return nil, errors.New("hooks is not an object")
			}
			for event, additions := range value.(map[string]any) {
				entries, ok := hooks[event].([]any)
				if hooks[event] == nil {
					continue
				}
				if !ok {
					return nil, errors.New("hook event is not an array")
				}
				kept := []any{}
				for _, entry := range entries {
					owned := false
					for _, addition := range additions.([]any) {
						owned = owned || equalJSON(entry, addition)
					}
					if !owned {
						kept = append(kept, entry)
					}
				}
				hooks[event] = kept
			}
		default:
			return nil, errors.New("unexpected installation patch key")
		}
	}
	data, err := json.MarshalIndent(current, "", "  ")
	return append(data, '\n'), err
}

func addMCPFragment(before []byte, fragment string) ([]byte, error) {
	name, wanted, err := mcpFragmentTable(fragment)
	if err != nil {
		return nil, err
	}
	current, err := mcpNamedTable(before, name)
	if err != nil {
		return nil, err
	}
	if current != nil {
		if !equalJSON(current, wanted) {
			return nil, errors.New("existing zalki MCP table differs; review it before replacing")
		}
		return before, nil
	}
	after := append(append([]byte{}, before...), []byte(fragment)...)
	if _, err := mcpNamedTable(after, name); err != nil {
		return nil, fmt.Errorf("cannot append zalki TOML safely: %w", err)
	}
	return after, nil
}

func removeMCPFragment(before []byte, fragment string) ([]byte, error) {
	name, wanted, err := mcpFragmentTable(fragment)
	if err != nil {
		return nil, err
	}
	current, err := mcpNamedTable(before, name)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return before, nil
	}
	if !equalJSON(current, wanted) || strings.Count(string(before), fragment) != 1 {
		return nil, errors.New("managed zalki MCP table was edited; review it before removing or upgrading")
	}
	after := []byte(strings.Replace(string(before), fragment, "", 1))
	remaining, err := mcpNamedTable(after, name)
	if err != nil || remaining != nil {
		return nil, errors.New("cannot remove zalki TOML safely; review the table manually")
	}
	return after, nil
}

func mcpFragmentTable(fragment string) (string, map[string]any, error) {
	var root map[string]any
	if err := toml.Unmarshal([]byte(fragment), &root); err != nil {
		return "", nil, fmt.Errorf("invalid recorded MCP fragment: %w", err)
	}
	servers, ok := root["mcp_servers"].(map[string]any)
	if !ok || len(root) != 1 {
		return "", nil, errors.New("invalid recorded MCP fragment")
	}
	name, err := managedMCPName(servers)
	if err != nil {
		return "", nil, err
	}
	return name, servers[name].(map[string]any), nil
}

func mcpNamedTable(data []byte, name string) (map[string]any, error) {
	var root map[string]any
	if err := toml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("invalid MCP TOML: %w", err)
	}
	if root["mcp_servers"] == nil {
		return nil, nil
	}
	servers, ok := root["mcp_servers"].(map[string]any)
	if !ok {
		return nil, errors.New("mcp_servers must be a table")
	}
	if servers[name] == nil {
		return nil, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok {
		return nil, errors.New("zalki MCP must be a table")
	}
	return entry, nil
}

func applyConfigEdits(edits []configEdit) error {
	stamp := backupStamp()
	for i := range edits {
		e := &edits[i]
		current, exists, err := configBytes(e.path)
		if err != nil {
			return err
		}
		if exists != e.existed || !bytes.Equal(current, e.before) {
			return fmt.Errorf("%s changed during planning; rerun the command", e.path)
		}
		if err := os.MkdirAll(filepath.Dir(e.path), 0700); err != nil {
			return err
		}
		if exists {
			e.backup = e.path + ".zalki-backup-" + stamp
			if err := fsutil.WriteExclusive(e.backup, e.before); err != nil {
				return err
			}
		}
	}
	for i, edit := range edits {
		var err error
		if edit.remove {
			err = os.Remove(edit.path)
		} else {
			err = fsutil.WriteAtomic(edit.path, edit.after)
		}
		if err != nil {
			var rollback error
			for _, prior := range edits[:i+1] {
				if prior.existed {
					rollback = errors.Join(rollback, fsutil.WriteAtomic(prior.path, prior.before))
				} else if e := os.Remove(prior.path); e != nil && !errors.Is(e, os.ErrNotExist) {
					rollback = errors.Join(rollback, e)
				}
			}
			return errors.Join(err, rollback)
		}
	}
	return nil
}
