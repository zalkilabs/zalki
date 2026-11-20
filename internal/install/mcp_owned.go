package install

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The managed zalki MCP entry holds keys that zalki writes (owned keys) and,
// possibly, keys the user added. An owned key is any key of a recorded entry or
// of the next declared entry. Upgrade rewrites owned keys only and keeps every
// other key. It refuses when an owned key was edited or removed, or when the
// user already set a key that zalki now owns to another value.

// ownedKeyProblem says why owned keys cannot be merged into an existing entry.
// edited means an owned key was changed or removed. Otherwise the user set a
// key that zalki now owns to another value.
type ownedKeyProblem struct {
	key    string
	edited bool
}

// ownedPlan lists the owned keys to write (set) and the keys of the matched
// recorded entry that the next entry no longer has (remove).
type ownedPlan struct {
	set    []string
	remove []string
}

// holdsAll reports whether current has every key of expected with an equal value.
func holdsAll(current, expected map[string]any) bool {
	for key, value := range expected {
		if have, ok := current[key]; !ok || !equalJSON(have, value) {
			return false
		}
	}
	return true
}

// matchOwned returns the first recorded entry that current still holds in full.
// An empty recorded entry never matches.
func matchOwned(current map[string]any, versions []map[string]any) (map[string]any, bool) {
	for _, version := range versions {
		if len(version) > 0 && holdsAll(current, version) {
			return version, true
		}
	}
	return nil, false
}

// firstDifference names the first owned key that current lacks or changed.
func firstDifference(current map[string]any, versions []map[string]any, wanted map[string]any) string {
	expected := wanted
	for _, version := range versions {
		if len(version) > 0 {
			expected = version
			break
		}
	}
	for _, key := range slices.Sorted(maps.Keys(expected)) {
		if have, ok := current[key]; !ok || !equalJSON(have, expected[key]) {
			return key
		}
	}
	return ""
}

// planOwnedKeys applies the merge rule. current is the entry in the file,
// wanted is the next entry and versions are the recorded entries (previous
// record first, then pending records). It writes nothing.
func planOwnedKeys(current, wanted map[string]any, versions []map[string]any) (ownedPlan, *ownedKeyProblem) {
	version, matched := matchOwned(current, versions)
	if !matched && !holdsAll(current, wanted) {
		return ownedPlan{}, &ownedKeyProblem{key: firstDifference(current, versions, wanted), edited: true}
	}
	var plan ownedPlan
	for _, key := range slices.Sorted(maps.Keys(wanted)) {
		have, present := current[key]
		_, owned := version[key]
		switch {
		case present && equalJSON(have, wanted[key]):
			// Already right: an owned key, or a key the user set to the same value.
		case owned || !present:
			plan.set = append(plan.set, key)
		default:
			return ownedPlan{}, &ownedKeyProblem{key: key}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(version)) {
		if _, kept := wanted[key]; !kept {
			plan.remove = append(plan.remove, key)
		}
	}
	return plan, nil
}

// ownedKeyError is the one-line refusal for a problem. label names the entry
// and noun is "entry" or "table". Without recorded entries this is an install
// over an entry that zalki did not write.
func ownedKeyError(problem *ownedKeyProblem, label, noun string, upgrading bool) error {
	switch {
	case !upgrading:
		return fmt.Errorf("an existing zalki %s differs in key %q; run zalki upgrade, or delete the %s and run zalki install again", noun, problem.key, noun)
	case problem.edited:
		return fmt.Errorf("%s key %q was edited after Zalki wrote it; restore it or delete that %s, then run zalki upgrade again", label, problem.key, noun)
	default:
		return fmt.Errorf("%s key %q is already set by you to another value; remove it or delete that %s, then run zalki upgrade again", label, problem.key, noun)
	}
}

// ownedEntries returns the zalki MCP entry of each recorded patch.
func ownedEntries(patches []map[string]any) []map[string]any {
	var entries []map[string]any
	for _, patch := range patches {
		servers, _ := patch["mcpServers"].(map[string]any)
		if entry, ok := servers[zalkiMCPServer].(map[string]any); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// mergeOwnedEntry returns the zalki entry that results from merging wanted into
// existing. The JSON value is a map of owned keys plus the user's keys.
func mergeOwnedEntry(existing, wanted any, versions []map[string]any) (any, error) {
	have, haveOK := existing.(map[string]any)
	want, wantOK := wanted.(map[string]any)
	if !haveOK || !wantOK {
		if equalJSON(existing, wanted) {
			return existing, nil
		}
		return nil, errors.New("an existing zalki MCP server differs; review it before replacing")
	}
	plan, problem := planOwnedKeys(have, want, versions)
	if problem != nil {
		return nil, ownedKeyError(problem, "mcpServers.zalki", "entry", len(versions) > 0)
	}
	merged := maps.Clone(have)
	for _, key := range plan.set {
		merged[key] = want[key]
	}
	for _, key := range plan.remove {
		delete(merged, key)
	}
	return merged, nil
}

// extraKeys lists the keys of have that owned does not have.
func extraKeys(have, owned map[string]any) []string {
	var extra []string
	for _, key := range slices.Sorted(maps.Keys(have)) {
		if _, ok := owned[key]; !ok {
			extra = append(extra, key)
		}
	}
	return extra
}

// onlyUserKeysDiffer returns the user's keys when one recorded entry is fully
// present in have and the entry differs only by keys the user added.
func onlyUserKeysDiffer(have map[string]any, versions []map[string]any) ([]string, bool) {
	for _, version := range versions {
		if len(version) == 0 || !holdsAll(have, version) {
			continue
		}
		if extra := extraKeys(have, version); len(extra) > 0 {
			return extra, true
		}
	}
	return nil, false
}

// userKeysRefusal is what uninstall does with an entry that holds keys the user
// added: it refuses and names them. This is the only place that decides it. To
// remove such an entry anyway, return the keys in a Report note here instead of
// an error; the installer already writes a backup of the file before each edit.
func userKeysRefusal(label, noun string, extra []string) error {
	return fmt.Errorf("%s has keys you added (%s); remove that %s by hand, then run zalki uninstall again", label, strings.Join(extra, ", "), noun)
}

// editedEntryRefusal is the uninstall refusal for a JSON entry that is not
// exactly one of the recorded entries.
func editedEntryRefusal(found any, owned []any) error {
	if have, ok := found.(map[string]any); ok {
		var versions []map[string]any
		for _, entry := range owned {
			if version, isMap := entry.(map[string]any); isMap {
				versions = append(versions, version)
			}
		}
		if extra, only := onlyUserKeysDiffer(have, versions); only {
			return userKeysRefusal("mcpServers.zalki", "entry", extra)
		}
	}
	return fmt.Errorf("managed %s MCP entry was edited; review it before removing or upgrading", zalkiMCPServer)
}
