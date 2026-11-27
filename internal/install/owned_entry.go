package install

import (
	"errors"
	"fmt"
	"slices"
)

// A file with a declared OwnedEntry holds one entry that zalki owns as a whole,
// under one top-level key. The installer reads and writes that key only. It does
// not know what the entry holds: what is in it, and how its values are laid out,
// is the adapter's declaration. The rules are those of the zalki MCP entry: an
// entry that equals a recorded entry, or one plus keys the user added, is zalki's;
// an upgrade updates it key by key and keeps the user's keys; an uninstall removes
// it and names the keys the user had added; an owned key that was edited, or an
// entry that is not an object, is refused.

// ownedEntryWording is what the installer says about the owned entry of name.
func ownedEntryWording(name string) entryWording {
	return entryWording{
		label:   fmt.Sprintf("entry %q", name),
		noun:    "entry",
		differs: fmt.Sprintf("entry %q is not a JSON object; delete it, then run zalki install again", name),
		edited:  fmt.Sprintf("entry %q was edited; delete it, then run zalki uninstall again", name),
	}
}

// validateRecordedPatch checks a patch read from an installation record against
// the declaration of its file.
func validateRecordedPatch(file ConfigFile, patch map[string]any) error {
	if file.OwnedEntry == "" {
		return validateInstallationPatch(patch)
	}
	entry, isObject := patch[file.OwnedEntry].(map[string]any)
	if len(patch) != 1 || !isObject || len(entry) == 0 {
		return fmt.Errorf("invalid recorded entry; expected exactly the %q entry", file.OwnedEntry)
	}
	return nil
}

// ownedRootEntries returns the owned entry of each recorded patch.
func ownedRootEntries(patches []map[string]any, name string) []any {
	var entries []any
	for _, patch := range patches {
		if entry, ok := patch[name].(map[string]any); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// planOwnedEntryFile plans a JSON file with a declared owned entry. An uninstall
// removes the recorded entry. An install or an upgrade merges the next entry; an
// upgrade also accepts an entry that still holds a recorded one.
func planOwnedEntryFile(change settingsChange, action string, before []byte, exists bool) ([]byte, []editNote, error) {
	name, wording := change.ownedEntry, ownedEntryWording(change.ownedEntry)
	after := before
	var notes []editNote
	owned := ownedRootEntries(slices.DeleteFunc(slices.Clone(change.ownedPatches), func(p map[string]any) bool { return p == nil }), name)
	if action == "uninstall" && len(owned) > 0 && exists {
		var current map[string]any
		if err := decodeSettings(after, &current); err != nil || current == nil {
			return nil, nil, fmt.Errorf("%s: %w", change.path, errors.New("settings must be a JSON object"))
		}
		userKeys, err := removeOwnedEntry(current, name, owned, wording)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", change.path, err)
		}
		if after, err = encodeSettings(current); err != nil {
			return nil, nil, err
		}
		notes = userKeysNotes(change.path, wording.label, wording.noun, slices.Compact(slices.Sorted(slices.Values(userKeys))))
	}
	if change.nextPatch != nil {
		var versions []map[string]any
		if action == "upgrade" {
			versions = recordedEntries(owned)
		}
		current, err := decodeSettingsToMerge(after)
		if err == nil {
			err = mergeOwnedInto(current, name, change.nextPatch[name], versions, wording)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", change.path, err)
		}
		if after, err = encodeSettings(current); err != nil {
			return nil, nil, err
		}
	}
	return after, notes, nil
}
