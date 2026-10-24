package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

type installationPlan struct {
	report InstallationReport
	edits  []configEdit
}

type installationManifest struct {
	path   string
	before []byte
	exists bool
	record installation
}

// Planning reads and validates all affected files. Only manageInstallation
// applies the resulting edits, after rebuilding the plan under its lock.
func (r *Registry) planInstallation(request InstallationRequest) (installationPlan, error) {
	plan := installationPlan{report: InstallationReport{Action: request.Action, DryRun: !request.Apply, Paths: []string{}}}
	if request.Action != "install" && request.Action != "upgrade" && request.Action != "uninstall" {
		return plan, errors.New("unsupported installation action")
	}
	if !filepath.IsAbs(request.StateDir) || !filepath.IsAbs(request.HomeDir) {
		return plan, errors.New("home and state paths must be absolute")
	}
	allowed, err := r.configuration("/zalki", request.StateDir)
	if err != nil {
		return plan, err
	}
	declarations := make(map[string]ConfigFile, len(allowed))
	for _, file := range allowed {
		declarations[file.Path] = file
	}
	manifest, err := readInstallationManifest(request, declarations)
	if err != nil {
		return plan, err
	}
	if request.Action == "upgrade" && !manifest.exists {
		return plan, errors.New("no installation record; run install --apply with the currently configured binary first")
	}
	if request.Action == "uninstall" && !manifest.exists {
		return plan, nil
	}
	next, err := r.desiredInstallation(request, declarations, manifest.path)
	if err != nil {
		return plan, err
	}
	if request.Action == "install" && manifest.exists && (manifest.record.Binary != request.Binary || !equalJSON(manifest.record.Targets, next.Targets)) {
		return plan, errors.New("zalki is installed at another binary or configuration path; use upgrade --apply")
	}
	plan.edits, err = planSettingsEdits(request.Action, manifest.record, next, declarations)
	if err != nil {
		return plan, err
	}
	for _, edit := range plan.edits {
		plan.report.Paths = append(plan.report.Paths, edit.path)
	}
	if request.Action == "uninstall" {
		plan.edits = append(plan.edits, configEdit{path: manifest.path, before: manifest.before, existed: true, remove: true})
	} else {
		data, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return plan, err
		}
		data = append(data, '\n')
		if !bytes.Equal(manifest.before, data) {
			plan.edits = append(plan.edits, configEdit{path: manifest.path, before: manifest.before, after: data, existed: manifest.exists})
		}
	}
	return plan, nil
}

func readInstallationManifest(request InstallationRequest, declarations map[string]ConfigFile) (installationManifest, error) {
	manifest := installationManifest{path: filepath.Join(request.StateDir, "installation.json")}
	var err error
	manifest.before, manifest.exists, err = configBytes(manifest.path)
	if err != nil || !manifest.exists {
		return manifest, err
	}
	if err := json.Unmarshal(manifest.before, &manifest.record); err != nil {
		return manifest, fmt.Errorf("read installation record: %w", err)
	}
	return manifest, manifest.record.validate(request, declarations, manifest.path)
}

func (record installation) validate(request InstallationRequest, declarations map[string]ConfigFile, manifestPath string) error {
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

func (r *Registry) desiredInstallation(request InstallationRequest, declarations map[string]ConfigFile, manifestPath string) (installation, error) {
	next := installation{Schema: 3, Home: request.HomeDir, Binary: request.Binary, State: request.StateDir,
		Patches: map[string]map[string]any{}, Fragments: map[string]string{}, Targets: map[string]string{}}
	if request.Action == "uninstall" {
		return next, nil
	}
	files, err := r.configuration(request.Binary, request.StateDir)
	if err != nil {
		return next, err
	}
	for _, file := range files {
		target, err := file.target(request.HomeDir)
		if err != nil {
			return next, err
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

func planSettingsEdits(action string, previous, next installation, declarations map[string]ConfigFile) ([]configEdit, error) {
	selected := map[string]bool{}
	previousKeys, nextKeys := map[string]string{}, map[string]string{}
	for key, path := range previous.Targets {
		selected[path] = true
		previousKeys[path] = key
	}
	for key, path := range next.Targets {
		selected[path] = true
		nextKeys[path] = key
		if oldKey, exists := previousKeys[path]; exists && (declarations[oldKey].JSON != nil) != (declarations[key].JSON != nil) {
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
		previousKey, nextKey := previousKeys[path], nextKeys[path]
		key := nextKey
		if key == "" {
			key = previousKey
		}
		change := settingsChange{path: path, isJSON: declarations[key].JSON != nil,
			previousPatch: previous.Patches[previousKey], nextPatch: next.Patches[nextKey],
			previousFragment: previous.Fragments[previousKey], nextFragment: next.Fragments[nextKey]}
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

type settingsChange struct {
	path                           string
	isJSON                         bool
	previousPatch, nextPatch       map[string]any
	previousFragment, nextFragment string
}

func (change settingsChange) plan(action string) (configEdit, error) {
	before, exists, err := configBytes(change.path)
	edit := configEdit{path: change.path, before: before, after: before, existed: exists}
	if err != nil {
		return edit, err
	}
	if change.isJSON {
		if change.previousPatch != nil && action != "install" && exists {
			edit.after, err = removeConfig(edit.after, change.previousPatch)
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
		if change.previousFragment != "" && action != "install" && exists {
			edit.after, err = removeMCPFragment(edit.after, change.previousFragment)
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
