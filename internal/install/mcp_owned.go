package install

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// The managed zalki MCP entry holds keys that zalki writes (owned keys) and,
// possibly, keys the user added. An owned key is any key of a recorded entry or
// of the next declared entry. Upgrade rewrites owned keys only and keeps every
// other key. It refuses when an owned key was edited or removed, or when the
// user already set a key that zalki now owns to another value.

// entryWording is what the owned-entry helpers say about one kind of entry:
// label names it in a message ("mcpServers.zalki"), noun is "entry" or "table",
// differs is the refusal for an existing value that is not an object, and
// edited is the refusal when no owned key can be named.
type entryWording struct{ label, noun, differs, edited string }

// mcpEditedText is the generic refusal for an edited zalki MCP entry or table.
func mcpEditedText(noun string) string {
	return fmt.Sprintf("managed %s MCP %s was edited; review it before removing or upgrading", zalkiMCPServer, noun)
}

// mcpEntryWording is the wording for the zalki entry of a JSON MCP file.
var mcpEntryWording = entryWording{
	label: "mcpServers.zalki", noun: "entry",
	differs: "an existing zalki MCP server differs; review it before replacing",
	edited:  mcpEditedText("entry"),
}

// mcpTableWording is the wording for the zalki table of a TOML MCP file.
var mcpTableWording = entryWording{label: "[mcp_servers.zalki]", noun: "table", edited: mcpEditedText("table")}

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
func mergeOwnedEntry(existing, wanted any, versions []map[string]any, w entryWording) (any, error) {
	have, haveOK := existing.(map[string]any)
	want, wantOK := wanted.(map[string]any)
	if !haveOK || !wantOK {
		if equalJSON(existing, wanted) {
			return existing, nil
		}
		return nil, errors.New(w.differs)
	}
	plan, problem := planOwnedKeys(have, want, versions)
	if problem != nil {
		return nil, ownedKeyError(problem, w.label, w.noun, len(versions) > 0)
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

// editNote is one sentence about the edit of one file, for the owner. A
// preview says what would happen and a result says what happened.
type editNote struct{ preview, result string }

// maxListedUserKeys and maxUserKeyName bound what one note prints.
const (
	maxListedUserKeys = 20
	maxUserKeyName    = 40
)

// plainKeyRune reports a character that a key name may hold without quoting.
func plainKeyRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.'
}

// userKeyName prints a key name from a user file. A plain name stays as it is.
// Any other name is quoted, so control characters never reach the terminal, and
// a long name is cut.
func userKeyName(key string) string {
	if key != "" && len(key) <= maxUserKeyName && strings.IndexFunc(key, func(r rune) bool { return !plainKeyRune(r) }) < 0 {
		return key
	}
	if len(key) > maxUserKeyName {
		return strconv.Quote(key[:maxUserKeyName]) + "..."
	}
	return strconv.Quote(key)
}

// listUserKeys prints the key names of a note: plain names, at most
// maxListedUserKeys of them, and the number of the rest.
func listUserKeys(keys []string) string {
	shown := keys[:min(len(keys), maxListedUserKeys)]
	names := make([]string, len(shown))
	for i, key := range shown {
		names[i] = userKeyName(key)
	}
	list := strings.Join(names, ", ")
	if more := len(keys) - len(shown); more > 0 {
		list += fmt.Sprintf(", and %d more", more)
	}
	return list
}

// userKeysNotes is the uninstall note about the user keys removed with the
// managed entry of one file; it is empty when there are none. The note lists
// names only. The backup stamp is random, so the note names the backup with a star.
func userKeysNotes(path, label, noun string, keys []string) []editNote {
	if len(keys) == 0 {
		return nil
	}
	list := listUserKeys(keys)
	return []editNote{{
		preview: fmt.Sprintf("%s: %s has keys you added (%s). Uninstall would remove the whole %s with them. Values are not shown. A backup named %s.zalki-backup-* will keep the old file.", path, label, list, noun, path),
		result:  fmt.Sprintf("%s: %s had keys you added (%s). Uninstall removed the whole %s with them. Values are not shown. The backup %s.zalki-backup-* keeps the old file.", path, label, list, noun, path),
	}}
}

// recordedEntries returns the recorded JSON entries that are objects.
func recordedEntries(owned []any) []map[string]any {
	var versions []map[string]any
	for _, entry := range owned {
		if version, isMap := entry.(map[string]any); isMap {
			versions = append(versions, version)
		}
	}
	return versions
}

// entryUserKeys decides what uninstall does with a JSON entry that is not
// exactly a recorded entry. When it is one recorded entry plus keys the user
// added, uninstall removes it and returns those key names. Otherwise it refuses.
func entryUserKeys(found any, owned []any, w entryWording) ([]string, error) {
	have, ok := found.(map[string]any)
	if !ok {
		return nil, editedKeyError(nil, nil, w)
	}
	versions := recordedEntries(owned)
	if extra, only := onlyUserKeysDiffer(have, versions); only {
		return extra, nil
	}
	return nil, editedKeyError(have, versions, w)
}

// editedKeyError is the uninstall refusal for an entry that is neither a
// recorded entry nor one of them plus the user's keys. An edited owned key may
// mean the entry is no longer zalki's, so it stays refused. The error names the
// first owned key that differs. With no recorded key to name (an empty recorded
// entry, or an entry that is not an object) the text is generic.
func editedKeyError(have map[string]any, versions []map[string]any, w entryWording) error {
	if key := firstDifference(have, versions, nil); key != "" {
		return fmt.Errorf("%s key %q was edited after Zalki wrote it; restore it or delete that %s, then run zalki uninstall again", w.label, key, w.noun)
	}
	return errors.New(w.edited)
}
