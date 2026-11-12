package install

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
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
	if request.Action == "upgrade" && !known {
		return plan, errors.New("no installation record; run install --apply with the currently configured binary first")
	}
	if request.Action == "uninstall" && !known {
		return plan, nil
	}
	next, err := declare.desired(request, declarations, manifest.path, previous.Targets)
	if err != nil {
		return plan, err
	}
	plan.report.Notes = keptTargetNotes(request, declarations, next.Targets)
	if err := checkInstallStart(request, manifest, next); err != nil {
		return plan, err
	}
	plan.edits, err = planSettingsEdits(request.Action, slices.Concat([]installation{previous}, manifest.pending), next, declarations)
	if err != nil {
		return plan, err
	}
	for _, edit := range plan.edits {
		plan.report.Paths = append(plan.report.Paths, edit.path)
	}
	plan.pendingPath = filepath.Join(request.StateDir, pendingRecordName)
	plan.clearStale = len(manifest.pending) > 0
	return plan, plan.addRecordEdits(request.Action, manifest, next)
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
	if action == "uninstall" {
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
	if manifest.exists && (manifest.record.Binary != request.Binary || !equalJSON(manifest.record.Targets, next.Targets)) {
		return errors.New("zalki is installed at another binary or configuration path; run zalki upgrade --config-only")
	}
	if len(manifest.pending) > 0 && !slices.ContainsFunc(manifest.pending, func(p installation) bool { return equalJSON(p, next) }) {
		return errors.New("an earlier zalki upgrade did not finish; run zalki upgrade")
	}
	return nil
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
		return manifest, err
	}
	pendingPath := filepath.Join(request.StateDir, pendingRecordName)
	_, found, err := readRecord(pendingPath, &manifest.pending)
	if err == nil && found && len(manifest.pending) == 0 {
		err = errors.New("installation pending record is empty")
	}
	for _, record := range manifest.pending {
		err = cmp.Or(err, record.validate(request, declarations, pendingPath))
	}
	return manifest, err
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
		if err := validateInstallationPatch(patch); err != nil {
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
func (declare Declare) desired(request Request, declarations map[string]ConfigFile, manifestPath string, recorded map[string]string) (installation, error) {
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

// planSettingsEdits plans every client file. owners[0] is the previous
// record and the rest are pending records: an entry equal to any of them is
// zalki's own and may be removed.
func planSettingsEdits(action string, owners []installation, next installation, declarations map[string]ConfigFile) ([]configEdit, error) {
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
			return nil, errors.New("configuration targets have incompatible file formats")
		}
	}
	paths := make([]string, 0, len(selected))
	for path := range selected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var edits []configEdit
	for _, path := range paths {
		change := settingsChange{path: path, nextPatch: next.Patches[nextKeys[path]], nextFragment: next.Fragments[nextKeys[path]]}
		key := nextKeys[path]
		for i, owner := range owners {
			ownerKey := ownerKeys[i][path]
			key = cmp.Or(key, ownerKey)
			change.ownedPatches = append(change.ownedPatches, owner.Patches[ownerKey])
			change.ownedFragments = append(change.ownedFragments, owner.Fragments[ownerKey])
		}
		change.isJSON = declarations[key].JSON != nil
		edit, err := change.plan(action)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(edit.before, edit.after) {
			edits = append(edits, edit)
		}
	}
	return edits, nil
}

// settingsChange plans one client file. The owned lists hold the previous
// record's entry and, after crashes, each pending record's entry; any one of
// them may be in the file.
type settingsChange struct {
	path           string
	isJSON         bool
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
	if change.isJSON {
		if owned := slices.DeleteFunc(slices.Clone(change.ownedPatches), func(p map[string]any) bool { return p == nil }); len(owned) > 0 && action != "install" && exists {
			edit.after, err = removeConfig(edit.after, owned...)
			if err != nil {
				return edit, fmt.Errorf("%s: %w", change.path, err)
			}
		}
		if change.nextPatch != nil {
			edit.after, err = mergeConfig(edit.after, change.nextPatch)
			if err != nil {
				return edit, fmt.Errorf("%s: %w", change.path, err)
			}
		}
	} else {
		if owned := slices.DeleteFunc(slices.Clone(change.ownedFragments), func(f string) bool { return f == "" }); len(owned) > 0 && action != "install" && exists {
			edit.after, err = removeMCPFragment(edit.after, owned...)
			if err != nil {
				return edit, err
			}
		}
		if change.nextFragment != "" {
			edit.after, err = addMCPFragment(edit.after, change.nextFragment)
			if err != nil {
				return edit, err
			}
		}
	}
	return edit, nil
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
