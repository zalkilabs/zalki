package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
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
	Action     string
	Binary     string
	StateDir   string
	HomeDir    string
	ConfigFile string
	// ExpectRecord binds a login-item change to what the owner was shown:
	// the LoginItemRecord of that inspect. A relaunch needs it; with it, an
	// install or relaunch refuses before any write when the item changed.
	ExpectRecord string

	Apply bool
}

type Report struct {
	Action string   `json:"action"`
	DryRun bool     `json:"dry_run"`
	Paths  []string `json:"paths"`
	Notes  []string `json:"notes,omitempty"`
	// Login item inspect only (ManagedLoginItem); empty for native installs.
	ManagedState     string `json:"managed_state,omitempty"`
	ServiceState     string `json:"service_state,omitempty"`
	LoadedIdentity   string `json:"loaded_identity,omitempty"`
	OperationPending bool   `json:"operation_pending,omitempty"`
	// LoginItemLauncher is the launcher path of the validated, owned login item record
	// (read at inspect time, never stored); empty when there is none.
	LoginItemLauncher string `json:"login_item_launcher,omitempty"`
	// LoginItemRecord is an opaque identity of that record ("GEN:SHA256"),
	// or "none" when there is no item at all; empty otherwise. It is only
	// passed back as Request.ExpectRecord and never encoded or printed.
	LoginItemRecord string `json:"-"`
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
	if request.Action == "uninstall" && len(plan.edits) == 0 && plan.pendingPath == "" {
		plan.report.DryRun = false
		return plan.report, nil
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
	return plan.report, plan.apply()
}

// apply writes the pending list, then the client files and the record, and
// then removes the pending list. After a failed write the list stays, even
// when the rollback worked: the next upgrade or uninstall accepts the entries
// of every record in it, and only its success removes it.
func (plan installationPlan) apply() error {
	if err := plan.writePending(); err != nil {
		return err
	}
	if err := applyConfigEdits(plan.edits); err != nil {
		return err
	}
	if plan.pending != nil || plan.clearStale {
		return removePendingRecord(plan.pendingPath)
	}
	return nil
}

func (plan installationPlan) writePending() error {
	if plan.pending == nil {
		return nil
	}
	// The pending list covers a process crash between writes, not a power
	// loss. A new state directory is still synced into its parent.
	state := filepath.Dir(plan.pendingPath)
	if _, err := os.Lstat(state); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(state, 0700); err != nil {
			return err
		}
		if err := fsutil.SyncDir(filepath.Dir(state)); err != nil {
			return err
		}
	}
	return fsutil.WriteAtomic(plan.pendingPath, plan.pending)
}

func removePendingRecord(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(path))
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

// equalJSON reports whether a and b encode the same JSON value. Numbers are
// compared by value, so a user's 5.0 or 5e0 equals the 5 zalki wrote, while
// the file keeps its own spelling.
func equalJSON(a, b any) bool {
	x, errX := decodedJSON(a)
	y, errY := decodedJSON(b)
	return errX == nil && errY == nil && sameJSON(x, y)
}

// decodedJSON turns any value into the generic JSON form with json.Number.
func decodedJSON(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result any
	err = decodeSettings(data, &result)
	return result, err
}

func sameJSON(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		return ok && sameNumber(x, y)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for key, value := range x {
			if other, found := y[key]; !found || !sameJSON(value, other) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		return ok && slices.EqualFunc(x, y, sameJSON)
	default:
		return a == b // string, bool or nil
	}
}

// sameNumber compares two JSON numbers with 256 bits of precision: exact for
// any integer up to 2^256 and for the decimals a settings file holds, and
// cheap even for a huge exponent.
func sameNumber(x, y json.Number) bool {
	if x == y {
		return true
	}
	f, okF := new(big.Float).SetPrec(256).SetString(string(x))
	g, okG := new(big.Float).SetPrec(256).SetString(string(y))
	return okF && okG && f.Cmp(g) == 0
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
// checked again here. With keepServer the zalki MCP entry stays in the file:
// an upgrade merges the next entry into it and keeps the user's own keys.
func removeConfig(before []byte, keepServer bool, path string, patches ...map[string]any) ([]byte, []editNote, error) {
	var current map[string]any
	if err := decodeSettings(before, &current); err != nil || current == nil {
		return nil, nil, errors.New("settings must be a JSON object")
	}
	var servers []any
	for _, patch := range patches {
		if owned, ok := patch["mcpServers"].(map[string]any); ok {
			servers = append(servers, owned[zalkiMCPServer])
		}
	}
	var userKeys []string
	for _, patch := range patches {
		for key, value := range patch {
			if keepServer && key == "mcpServers" {
				if err := checkServersObject(current); err != nil {
					return nil, nil, err
				}
				continue
			}
			removed, err := removePatchKey(current, key, value, servers)
			if err != nil {
				return nil, nil, err
			}
			userKeys = append(userKeys, removed...)
		}
	}
	after, err := encodeSettings(current)
	return after, userKeysNotes(path, "mcpServers.zalki", "entry", slices.Compact(slices.Sorted(slices.Values(userKeys)))), err
}

// removePatchKey removes what one top-level key of a recorded patch added.
// servers lists every zalki MCP entry that one of the patches recorded. It
// returns the names of the user's keys that went with a removed MCP entry.
func removePatchKey(current map[string]any, key string, value any, servers []any) ([]string, error) {
	switch key {
	case "version": // Cursor schema version is shared with unrelated hooks.
		return nil, nil
	case "mcpServers":
		return removeManagedMCPServer(current, servers)
	case "hooks":
		return nil, removeOwnedHooks(current, value.(map[string]any))
	default:
		return nil, errors.New("unexpected installation patch key")
	}
}

// removeManagedMCPServer deletes the zalki MCP server when it equals one of
// the owned entries, or one of them plus keys the user added; the user's keys
// go with it and their names are returned. Any other entry was edited in a key
// zalki owns and is refused.
func removeManagedMCPServer(current map[string]any, owned []any) ([]string, error) {
	if err := checkServersObject(current); err != nil {
		return nil, err
	}
	servers, ok := current["mcpServers"].(map[string]any)
	if !ok {
		return nil, nil // absent or null
	}
	found, exists := servers[zalkiMCPServer]
	if !exists {
		return nil, nil
	}
	var userKeys []string
	if !containsJSON(owned, found) {
		var err error
		if userKeys, err = entryUserKeys(found, owned); err != nil {
			return nil, err
		}
	}
	delete(servers, zalkiMCPServer)
	return userKeys, nil
}

// checkServersObject refuses an mcpServers value that is neither absent, null
// nor an object. An upgrade keeps the zalki entry but still refuses such a file here.
func checkServersObject(current map[string]any) error {
	if _, isObject := current["mcpServers"].(map[string]any); current["mcpServers"] != nil && !isObject {
		return errors.New("mcpServers is not an object")
	}
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

// decodeSettings reads one JSON value from a client settings file. Numbers
// stay json.Number, so a large integer or a float such as 1.50 is written back
// with the same digits.
func decodeSettings(data []byte, v any) error {
	if !json.Valid(data) {
		return json.Unmarshal(data, v) // reports the *json.SyntaxError
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(v)
}

// encodeSettings writes a settings object in the format the installer uses.
// It keeps the user's "&", "<" and ">" as they are. Keys are sorted.
func encodeSettings(current map[string]any) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(current)
	return b.Bytes(), err
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

// removeMCPFragment removes the zalki table when it equals one of the owned
// fragments and that fragment is in the file exactly once. A table that is one
// owned fragment plus keys the user added is removed whole, with its
// sub-tables, and a note names the user's keys.
func removeMCPFragment(before []byte, path string, fragments ...string) ([]byte, []editNote, error) {
	var name, fragment string
	var current map[string]any
	for _, owned := range fragments {
		tableName, wanted, err := mcpFragmentTable(owned)
		if err != nil {
			return nil, nil, err
		}
		if current, err = mcpNamedTable(before, tableName); err != nil {
			return nil, nil, err
		}
		if current == nil {
			return before, nil, nil
		}
		if equalJSON(current, wanted) && strings.Count(string(before), owned) == 1 {
			name, fragment = tableName, owned
			break
		}
	}
	if fragment == "" {
		userKeys, err := tableUserKeys(current, fragments)
		if err != nil {
			return nil, nil, err
		}
		after, err := removeZalkiTable(before)
		return after, userKeysNotes(path, "[mcp_servers.zalki]", "table", userKeys), err
	}
	after := []byte(strings.Replace(string(before), fragment, "", 1))
	remaining, err := mcpNamedTable(after, name)
	if err != nil || remaining != nil {
		return nil, nil, errors.New("cannot remove zalki TOML safely; review the table manually")
	}
	return after, nil, nil
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
