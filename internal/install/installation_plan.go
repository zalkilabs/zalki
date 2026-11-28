package install

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

type installationPlan struct {
	report Report
	edits  []configEdit
	// pending is written before the first client edit and removed after the
	// record, so a crash in between still names every entry zalki may own.
	pendingPath string
	pending     []byte
	clearStale  bool
}

type installationManifest struct {
	path   string
	before []byte
	exists bool
	record installation
	// pending lists the records of every upgrade or install that did not
	// finish since the last commit, oldest first.
	pending []installation
}

// pendingRecordName holds a JSON list of records while client files are
// written. A new run adds its record to the list and never removes one, so
// two crashes in a row still name every entry zalki may own.
const pendingRecordName = "installation.pending.json"

// Planning reads and validates all affected files. Only Manage
// applies the resulting edits, after rebuilding the plan under its lock.
func (declare Declare) plan(request Request) (installationPlan, error) {
	plan := installationPlan{report: Report{Action: request.Action, DryRun: !request.Apply, Paths: []string{}}}
	if request.Action != "install" && request.Action != "upgrade" && request.Action != "uninstall" {
		return plan, errors.New("unsupported installation action")
	}
	if !filepath.IsAbs(request.StateDir) || !filepath.IsAbs(request.HomeDir) {
		return plan, errors.New("home and state paths must be absolute")
	}
	declarations, err := declare.planDeclarations(request)
	if err != nil {
		return plan, err
	}
	manifest, err := readInstallationManifest(request, declarations)
	if err != nil {
		return plan, err
	}
	previous, known := manifest.previous(request.Action)
	if request.Action == "uninstall" && !known {
		return plan, nil
	}
	groups := clientGroups(declarations)
	absent, err := absentClients(request, declarations, groups)
	if err != nil {
		return plan, err
	}
	next, err := declare.desired(request, declarations, manifest.path, previous, absent)
	if err != nil {
		return plan, err
	}
	plan.report.Notes = keptTargetNotes(request, declarations, next.Targets)
	if err := checkInstallStart(request, manifest, next); err != nil {
		return plan, err
	}
	var failed map[string]string
	plan.edits, failed, err = planSettingsEdits(request.Action, slices.Concat([]installation{previous}, manifest.pending), next, declarations, absent)
	if err != nil {
		return plan, err
	}
	next = keepFailedClients(next, previous, groups, failed)
	for _, edit := range plan.edits {
		plan.report.Paths = append(plan.report.Paths, edit.path)
	}
	plan.report.Notes = append(plan.report.Notes, editNoteTexts(plan.edits, plan.report.DryRun)...)
	plan.report.Notes = append(plan.report.Notes, clientNotes(request.Action, groups, absent, failed, &plan.report)...)
	plan.pendingPath = filepath.Join(request.StateDir, pendingRecordName)
	plan.clearStale = len(manifest.pending) > 0
	return plan, plan.addRecordEdits(request.Action, manifest, next)
}

// editNoteTexts returns the owner's sentences about the planned edits: what
// would happen for a preview, what happened otherwise.
func editNoteTexts(edits []configEdit, dryRun bool) []string {
	var texts []string
	for _, edit := range edits {
		for _, note := range edit.notes {
			if dryRun {
				texts = append(texts, note.preview)
			} else {
				texts = append(texts, note.result)
			}
		}
	}
	return texts
}

// previous returns the record that lists what zalki may own now. A first
// install that stopped before its record has only the pending record; an
// install never removes, so it does not count as known there.
func (manifest installationManifest) previous(action string) (installation, bool) {
	if last := len(manifest.pending) - 1; !manifest.exists && last >= 0 {
		return manifest.pending[last], action != "install"
	}
	return manifest.record, manifest.exists
}

// addRecordEdits adds the record edit last, so it is the commit point. Any
// client edit before it is covered by the pending record.
func (plan *installationPlan) addRecordEdits(action string, manifest installationManifest, next installation) error {
	if action == "uninstall" && len(next.Targets) == 0 {
		if manifest.exists {
			plan.edits = append(plan.edits, configEdit{path: manifest.path, before: manifest.before, existed: true, remove: true})
		}
		return nil
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(plan.edits) > 0 {
		pending := manifest.pending
		if !slices.ContainsFunc(pending, func(p installation) bool { return equalJSON(p, next) }) {
			pending = append(slices.Clone(pending), next)
		}
		if plan.pending, err = json.MarshalIndent(pending, "", "  "); err != nil {
			return err
		}
		plan.pending = append(plan.pending, '\n')
	}
	if !bytes.Equal(manifest.before, data) {
		plan.edits = append(plan.edits, configEdit{path: manifest.path, before: manifest.before, after: data, existed: manifest.exists})
	}
	return nil
}

// checkInstallStart refuses an install over a different installation. A
// pending record from an earlier install with the same entries lets the
// install finish that job.
func checkInstallStart(request Request, manifest installationManifest, next installation) error {
	if request.Action != "install" {
		return nil
	}
	if manifest.exists && (manifest.record.Binary != request.Binary || !keepsRecordedTargets(manifest.record.Targets, next.Targets)) {
		return errors.New("zalki is installed at another binary or configuration path; run zalki upgrade --config-only")
	}
	if len(manifest.pending) > 0 && !slices.ContainsFunc(manifest.pending, func(p installation) bool { return equalJSON(p, next) }) {
		return errors.New("an earlier zalki upgrade did not finish; run zalki upgrade")
	}
	return nil
}

// keepsRecordedTargets reports whether next names every recorded file at the
// place the record has it. It may name more: an install run again after a client
// was skipped or appeared adds that client.
func keepsRecordedTargets(recorded, next map[string]string) bool {
	for key, target := range recorded {
		if next[key] != target {
			return false
		}
	}
	return true
}

func readInstallationManifest(request Request, declarations map[string]ConfigFile) (installationManifest, error) {
	manifest := installationManifest{path: filepath.Join(request.StateDir, "installation.json")}
	if err := fsutil.CheckPrivateDir(request.StateDir); errors.Is(err, os.ErrNotExist) {
		return manifest, nil
	} else if err != nil {
		return manifest, err
	}
	var err error
	manifest.before, manifest.exists, err = readRecord(manifest.path, &manifest.record)
	if err == nil && manifest.exists {
		err = manifest.record.validate(request, declarations, manifest.path)
	}
	if err != nil {
		return manifest, invalidRecord(request.Action, manifest.path, err)
	}
	pendingPath := filepath.Join(request.StateDir, pendingRecordName)
	_, found, err := readRecord(pendingPath, &manifest.pending)
	if err == nil && found && len(manifest.pending) == 0 {
		err = errors.New("installation pending record is empty")
	}
	for _, record := range manifest.pending {
		err = cmp.Or(err, record.validate(request, declarations, pendingPath))
	}
	return manifest, invalidRecord(request.Action, pendingPath, err)
}

func invalidRecord(action, path string, err error) error {
	if err == nil || action != "upgrade" {
		return err
	}
	return fmt.Errorf("installation record %q is invalid: %w. Restore a valid record before retrying.", path, err) //nolint:revive // error-strings: the exact CLI diagnostic contract includes a final period.
}

// readRecord reads one record file into value. The file must belong to the
// user and be closed to writes by others.
func readRecord(path string, value any) ([]byte, bool, error) {
	data, exists, err := configBytes(path)
	if err != nil || !exists {
		return data, exists, err
	}
	if err := checkPrivateFile(path); err != nil {
		return data, exists, err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return data, exists, fmt.Errorf("read installation record: %w", err)
	}
	return data, exists, nil
}

// checkPrivateFile refuses a record owned by another user or writable by
// group or others.
func checkPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("installation record must be owned by you and not writable by others: %s", path)
	}
	return nil
}

func (record installation) validate(request Request, declarations map[string]ConfigFile, manifestPath string) error {
	if record.Schema != 3 || record.Home != request.HomeDir || record.State != request.StateDir || !filepath.IsAbs(record.Binary) || len(record.Patches)+len(record.Fragments) == 0 {
		return errors.New("installation record does not match this home/state or has an unsupported schema")
	}
	for path, patch := range record.Patches {
		file, exists := declarations[path]
		if !exists || file.JSON == nil {
			return errors.New("installation record has unexpected settings paths")
		}
		if err := validateRecordedPatch(file, patch); err != nil {
			return err
		}
	}
	for path, fragment := range record.Fragments {
		file, exists := declarations[path]
		if !exists || file.JSON != nil {
			return errors.New("installation record has unexpected fragment paths")
		}
		if _, _, err := mcpFragmentTable(fragment); err != nil {
			return err
		}
	}
	return record.validateTargets(declarations, manifestPath)
}

// desired builds the next record. An upgrade keeps every target that the
// previous record has, so a shell with another CLAUDE_CONFIG_DIR or CODEX_HOME
// does not move the integration.
func (declare Declare) desired(request Request, declarations map[string]ConfigFile, manifestPath string, previous installation, absent map[string]bool) (installation, error) {
	recorded := previous.Targets
	next := installation{Schema: 3, Home: request.HomeDir, Binary: request.Binary, State: request.StateDir,
		Patches: map[string]map[string]any{}, Fragments: map[string]string{}, Targets: map[string]string{}}
	if request.Action == "uninstall" {
		return next, nil
	}
	files, err := declare(request.Binary, request.StateDir)
	if err != nil {
		return next, err
	}
	for _, file := range files {
		if file.Client != "" && absent[file.Client] {
			keepRecorded(&next, previous, file.Path)
			continue
		}
		target, err := file.target(request.HomeDir)
		if err != nil {
			return next, err
		}
		if kept, ok := recorded[file.Path]; ok && request.Action == "upgrade" {
			target = kept
		}
		next.Targets[file.Path] = target
		if file.JSON != nil {
			next.Patches[file.Path] = file.JSON
		} else {
			next.Fragments[file.Path] = file.TOML
		}
	}
	return next, next.validateTargets(declarations, manifestPath)
}

// keptTargetNotes names each kept upgrade target that the environment now
// points away from.
func keptTargetNotes(request Request, declarations map[string]ConfigFile, targets map[string]string) []string {
	var notes []string
	if request.Action != "upgrade" {
		return notes
	}
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		file := declarations[key]
		if now, err := file.target(request.HomeDir); err == nil && now != targets[key] {
			notes = append(notes, fmt.Sprintf("Kept %s (%s points elsewhere; to move, run zalki uninstall, then zalki install)", targets[key], file.ConfigRootEnv))
		}
	}
	return notes
}

// keepRecorded copies the previous record's entry for one file into next: the
// file is not touched by this run, so what zalki wrote there stays on record.
func keepRecorded(next *installation, previous installation, key string) {
	target, ok := previous.Targets[key]
	if !ok {
		return
	}
	next.Targets[key] = target
	if patch, ok := previous.Patches[key]; ok {
		next.Patches[key] = patch
	}
	if fragment, ok := previous.Fragments[key]; ok {
		next.Fragments[key] = fragment
	}
}

// absentClients finds the clients that are not on this machine. Nothing is
// skipped for an uninstall, which only removes what the record names. It
// refuses a run that would configure no client at all.
func absentClients(request Request, declarations map[string]ConfigFile, groups []clientGroup) (map[string]bool, error) {
	absent := map[string]bool{}
	if request.Action == "uninstall" || len(groups) == 0 {
		return absent, nil
	}
	present := 0
	for _, group := range groups {
		if group.present(request) {
			present++
		} else {
			absent[group.id] = true
		}
	}
	clientless := slices.ContainsFunc(slices.Collect(maps.Values(declarations)), func(file ConfigFile) bool { return file.Client == "" })
	if present == 0 && !clientless {
		return nil, noClientError(request.Action, groups)
	}
	return absent, nil
}

// keepFailedClients keeps the previous record's entries for the clients whose
// file could not be used: this run did not change their files.
func keepFailedClients(next, previous installation, groups []clientGroup, failed map[string]string) installation {
	for _, group := range groups {
		if _, bad := failed[group.id]; !bad {
			continue
		}
		for _, file := range group.files {
			delete(next.Targets, file.Path)
			delete(next.Patches, file.Path)
			delete(next.Fragments, file.Path)
			keepRecorded(&next, previous, file.Path)
		}
	}
	return next
}

// clientNotes are the lines about the clients this run skipped. It also fills
// the report's lists of configured and unusable clients.
func clientNotes(action string, groups []clientGroup, absent map[string]bool, failed map[string]string, report *Report) []string {
	var notes []string
	var missing []clientGroup
	for _, group := range groups {
		switch reason, bad := failed[group.id]; {
		case bad:
			notes = append(notes, skippedNotes(group, reason, action)...)
			report.Unusable = append(report.Unusable, labelOf(group))
		case absent[group.id]:
			missing = append(missing, group)
		default:
			report.Configured = append(report.Configured, group.id)
		}
	}
	if len(missing) > 0 {
		notes = append(notes, notFoundNote(missing))
	}
	return notes
}

// planSettingsEdits plans every client file. owners[0] is the previous
// record and the rest are pending records: an entry equal to any of them is
// zalki's own and may be removed.
func planSettingsEdits(action string, owners []installation, next installation, declarations map[string]ConfigFile, absent map[string]bool) ([]configEdit, map[string]string, error) {
	selected := map[string]bool{}
	ownerKeys, nextKeys := make([]map[string]string, len(owners)), map[string]string{}
	for i, owner := range owners {
		ownerKeys[i] = map[string]string{}
		for key, path := range owner.Targets {
			selected[path] = true
			ownerKeys[i][path] = key
		}
	}
	for key, path := range next.Targets {
		selected[path] = true
		nextKeys[path] = key
		if oldKey, exists := ownerKeys[0][path]; exists && (declarations[oldKey].JSON != nil) != (declarations[key].JSON != nil) {
			return nil, nil, errors.New("configuration targets have incompatible file formats")
		}
	}
	for path := range absentPaths(owners, next, declarations, absent) {
		delete(selected, path)
	}
	paths := make([]string, 0, len(selected))
	for path := range selected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var edits []configEdit
	failed, editClient := map[string]string{}, map[string]string{}
	for _, path := range paths {
		change, key := changeFor(path, owners, ownerKeys, next, nextKeys, declarations)
		edit, err := change.plan(action)
		if err != nil {
			client := declarations[key].Client
			if client == "" {
				return nil, nil, err
			}
			if _, seen := failed[client]; !seen {
				failed[client] = err.Error()
			}
			continue
		}
		if !bytes.Equal(edit.before, edit.after) {
			edits = append(edits, edit)
			editClient[path] = declarations[key].Client
		}
	}
	edits = slices.DeleteFunc(edits, func(edit configEdit) bool {
		_, bad := failed[editClient[edit.path]]
		return bad
	})
	return edits, failed, nil
}

// changeFor builds what one file needs: the entries the previous and pending
// records own there and the entry the next record wants. It also returns the
// file's key among the declarations.
func changeFor(path string, owners []installation, ownerKeys []map[string]string, next installation, nextKeys map[string]string, declarations map[string]ConfigFile) (settingsChange, string) {
	change := settingsChange{path: path, nextPatch: next.Patches[nextKeys[path]], nextFragment: next.Fragments[nextKeys[path]]}
	key := nextKeys[path]
	for i, owner := range owners {
		ownerKey := ownerKeys[i][path]
		key = cmp.Or(key, ownerKey)
		change.ownedPatches = append(change.ownedPatches, owner.Patches[ownerKey])
		change.ownedFragments = append(change.ownedFragments, owner.Fragments[ownerKey])
	}
	change.isJSON = declarations[key].JSON != nil
	change.ownedEntry = declarations[key].OwnedEntry
	return change, key
}

// absentPaths are the files of the clients this run does not configure: it
// neither reads nor edits them.
func absentPaths(owners []installation, next installation, declarations map[string]ConfigFile, absent map[string]bool) map[string]bool {
	paths := map[string]bool{}
	for key, file := range declarations {
		if file.Client == "" || !absent[file.Client] {
			continue
		}
		for _, owner := range owners {
			if path, ok := owner.Targets[key]; ok {
				paths[path] = true
			}
		}
		if path, ok := next.Targets[key]; ok {
			paths[path] = true
		}
	}
	return paths
}

// settingsChange plans one client file. The owned lists hold the previous
// record's entry and, after crashes, each pending record's entry; any one of
// them may be in the file.
type settingsChange struct {
	path   string
	isJSON bool
	// ownedEntry is the declared top-level key that zalki owns as a whole; empty
	// for a file merged by the rules of the built-in settings keys.
	ownedEntry     string
	ownedPatches   []map[string]any
	nextPatch      map[string]any
	ownedFragments []string
	nextFragment   string
}

func (change settingsChange) plan(action string) (configEdit, error) {
	before, exists, err := configBytes(change.path)
	edit := configEdit{path: change.path, before: before, after: before, existed: exists}
	if err != nil {
		return edit, err
	}
	switch {
	case change.isJSON && change.ownedEntry != "":
		edit.after, edit.notes, err = planOwnedEntryFile(change, action, before, exists)
	case change.isJSON:
		edit.after, edit.notes, err = planJSONFile(change, action, before, exists)
	default:
		edit.after, edit.notes, err = planTOMLFile(change, action, before, exists)
	}
	return edit, err
}

// planJSONFile plans a JSON client file. Hooks of the recorded entries are removed
// on upgrade and uninstall. The zalki MCP entry is removed only on uninstall, with
// any keys the user added to it (a note lists their names); an upgrade merges the
// next entry into it, and an install accepts an entry that already has every
// owned key.
func planJSONFile(change settingsChange, action string, before []byte, exists bool) ([]byte, []editNote, error) {
	after := before
	var notes []editNote
	owned := slices.DeleteFunc(slices.Clone(change.ownedPatches), func(p map[string]any) bool { return p == nil })
	var err error
	if len(owned) > 0 && action != "install" && exists {
		after, notes, err = removeConfig(after, action == "upgrade", change.path, owned...)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", change.path, err)
		}
	}
	if change.nextPatch != nil {
		versions := owned
		if action != "upgrade" {
			versions = nil
		}
		after, err = mergeConfig(after, change.nextPatch, versions, action)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", change.path, err)
		}
	}
	return after, notes, nil
}

// planTOMLFile plans a Codex TOML file. Uninstall removes the recorded table when it
// is exactly zalki's, or zalki's plus keys the user added (a note lists their names).
// An upgrade or install updates the managed table in place.
func planTOMLFile(change settingsChange, action string, before []byte, exists bool) ([]byte, []editNote, error) {
	after := before
	var notes []editNote
	owned := slices.DeleteFunc(slices.Clone(change.ownedFragments), func(f string) bool { return f == "" })
	var err error
	if action == "uninstall" && len(owned) > 0 && exists {
		after, notes, err = removeMCPFragment(after, change.path, owned...)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", change.path, err)
		}
	}
	if change.nextFragment != "" {
		versions := owned
		if action != "upgrade" {
			versions = nil
		}
		after, err = updateMCPFragment(change.path, after, versions, change.nextFragment)
		if err != nil {
			return nil, nil, err
		}
	}
	return after, notes, nil
}

func (declare Declare) planDeclarations(request Request) (map[string]ConfigFile, error) {
	allowed, err := declare("/zalki", request.StateDir)
	if err != nil {
		return nil, err
	}
	declarations := make(map[string]ConfigFile, len(allowed))
	for _, file := range allowed {
		if request.Action != "uninstall" {
			target, err := file.target(request.HomeDir)
			if err != nil {
				return nil, err
			}
			if err := validateReservedTarget(target, file, request); err != nil {
				return nil, err
			}
		}
		declarations[file.Path] = file
	}
	return declarations, nil
}
