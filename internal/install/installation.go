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

// validateInstallationPatch checks a patch read from an installation record.
// An empty patch is refused first. Then each key is checked in map order and
// the first fault is returned, so a patch with faults in two keys may be refused
// with either message.
func validateInstallationPatch(patch map[string]any) error {
	if len(patch) == 0 {
		return errors.New("empty installation patch")
	}
	for key, value := range patch {
		if err := validatePatchKey(key, value); err != nil {
			return err
		}
	}
	return nil
}

// validatePatchKey checks the value of one top-level key of a recorded patch.
func validatePatchKey(key string, value any) error {
	switch key {
	case "version":
		return validateRecordedVersion(value)
	case "mcpServers":
		return validateRecordedMCPServers(value)
	case "hooks":
		return validateRecordedHooks(value)
	default:
		return errors.New("unexpected recorded settings key")
	}
}

// validateRecordedVersion accepts only the hook schema version 1.
func validateRecordedVersion(value any) error {
	if value != float64(1) {
		return errors.New("unsupported recorded hook version")
	}
	return nil
}

// validateRecordedMCPServers accepts an object that holds exactly the zalki server.
func validateRecordedMCPServers(value any) error {
	servers, ok := value.(map[string]any)
	if !ok {
		return errors.New("invalid recorded MCP entry")
	}
	_, err := managedMCPName(servers)
	return err
}

// validateRecordedHooks accepts an object whose events are all non-empty lists.
func validateRecordedHooks(value any) error {
	hooks, ok := value.(map[string]any)
	if !ok {
		return errors.New("invalid recorded hooks")
	}
	for _, event := range hooks {
		if entries, ok := event.([]any); !ok || len(entries) == 0 {
			return errors.New("invalid recorded hook entries")
		}
	}
	return nil
}

// zalkiMCPServer is the name of the one MCP server the installer manages.
const zalkiMCPServer = "zalki"

func managedMCPName(servers map[string]any) (string, error) {
	if len(servers) == 1 {
		if _, ok := servers[zalkiMCPServer].(map[string]any); ok {
			return zalkiMCPServer, nil
		}
	}
	return "", errors.New("invalid recorded MCP entry; expected exactly one zalki server")
}

// removeConfig takes the entries recorded in patch out of a JSON settings
// file. It needs a patch that validateInstallationPatch has accepted: every
// caller reads the patch from an installation record that was validated when
// it was loaded (see readInstallationManifest), so the shape of patch is not
// checked again here.
func removeConfig(before []byte, patch map[string]any) ([]byte, error) {
	var current map[string]any
	if err := json.Unmarshal(before, &current); err != nil || current == nil {
		return nil, errors.New("settings must be a JSON object")
	}
	for key, value := range patch {
		if err := removePatchKey(current, key, value); err != nil {
			return nil, err
		}
	}
	return encodeSettings(current)
}

// removePatchKey removes what one top-level key of a recorded patch added.
func removePatchKey(current map[string]any, key string, value any) error {
	switch key {
	case "version": // Cursor schema version is shared with unrelated hooks.
		return nil
	case "mcpServers":
		return removeManagedMCPServer(current, value.(map[string]any))
	case "hooks":
		return removeOwnedHooks(current, value.(map[string]any))
	default:
		return errors.New("unexpected installation patch key")
	}
}

// removeManagedMCPServer deletes the zalki MCP server unless the user edited it.
func removeManagedMCPServer(current, owned map[string]any) error {
	if current["mcpServers"] == nil {
		return nil
	}
	servers, ok := current["mcpServers"].(map[string]any)
	if !ok {
		return errors.New("mcpServers is not an object")
	}
	found, exists := servers[zalkiMCPServer]
	if !exists {
		return nil
	}
	if !equalJSON(found, owned[zalkiMCPServer]) {
		return fmt.Errorf("managed %s MCP entry was edited; review it before removing or upgrading", zalkiMCPServer)
	}
	delete(servers, zalkiMCPServer)
	return nil
}

// removeOwnedHooks removes the recorded hook entries, event by event, and
// keeps every entry the user added.
func removeOwnedHooks(current, owned map[string]any) error {
	if current["hooks"] == nil {
		return nil
	}
	hooks, ok := current["hooks"].(map[string]any)
	if !ok {
		return errors.New("hooks is not an object")
	}
	for event, additions := range owned {
		if err := removeOwnedHookEntries(hooks, event, additions.([]any)); err != nil {
			return err
		}
	}
	return nil
}

// removeOwnedHookEntries drops the entries of one hook event that equal a
// recorded addition. An event that is left empty stays as an empty list.
func removeOwnedHookEntries(hooks map[string]any, event string, additions []any) error {
	if hooks[event] == nil {
		return nil
	}
	entries, ok := hooks[event].([]any)
	if !ok {
		return errors.New("hook event is not an array")
	}
	kept := []any{}
	for _, entry := range entries {
		if !containsJSON(additions, entry) {
			kept = append(kept, entry)
		}
	}
	hooks[event] = kept
	return nil
}

// containsJSON reports whether list has an item that encodes like wanted.
func containsJSON(list []any, wanted any) bool {
	for _, item := range list {
		if equalJSON(item, wanted) {
			return true
		}
	}
	return false
}

// encodeSettings writes a settings object in the format the installer uses.
func encodeSettings(current map[string]any) ([]byte, error) {
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

// applyConfigEdits checks and prepares every edit, then writes them in order.
// If a write fails, it attempts to restore the prefix including the failed edit.
func applyConfigEdits(edits []configEdit) error {
	if err := prepareConfigEdits(edits); err != nil {
		return err
	}
	for i, edit := range edits {
		if err := writeConfigEdit(edit); err != nil {
			return errors.Join(err, rollbackConfigEdits(edits[:i+1]))
		}
	}
	return nil
}

// prepareConfigEdits checks and prepares each edit in order, setting its backup
// path. Earlier directories and backups may remain if a later edit is refused.
func prepareConfigEdits(edits []configEdit) error {
	stamp := backupStamp()
	for i := range edits {
		if err := prepareConfigEdit(&edits[i], stamp); err != nil {
			return err
		}
	}
	return nil
}

// prepareConfigEdit prepares one edit: it checks that the file is unchanged,
// creates its directory and backs up the existing file.
func prepareConfigEdit(e *configEdit, stamp string) error {
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
		return fsutil.WriteExclusive(e.backup, e.before)
	}
	return nil
}

// writeConfigEdit removes the file or replaces it with the new content.
func writeConfigEdit(edit configEdit) error {
	if edit.remove {
		return os.Remove(edit.path)
	}
	return fsutil.WriteAtomic(edit.path, edit.after)
}

// rollbackConfigEdits restores the files of the given edits: files that
// existed get their old content, new files are removed. It returns every
// error it met, joined.
func rollbackConfigEdits(edits []configEdit) error {
	var rollback error
	for _, prior := range edits {
		if prior.existed {
			rollback = errors.Join(rollback, fsutil.WriteAtomic(prior.path, prior.before))
		} else if e := os.Remove(prior.path); e != nil && !errors.Is(e, os.ErrNotExist) {
			rollback = errors.Join(rollback, e)
		}
	}
	return rollback
}
