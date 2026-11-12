package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

var errOwnerOnly = errors.New("crossSessionInbound is owner-only. Zalki will not change it.") //nolint:revive // error-strings: the exact CLI diagnostic contract includes a final period.

// Update atomically sets one supported leaf in the selected private JSON file.
// A nil value removes its override. The result is the effective new snapshot,
// including flag/env precedence. Concurrent editors use last-writer-wins.
func Update(input Inputs, key string, value *string) (Snapshot, error) {
	if err := checkEditKey(key); err != nil {
		return Snapshot{}, err
	}
	input = captureInputs(input)
	paths, err := ResolvePaths(input)
	if err != nil {
		return Snapshot{}, err
	}
	raw, exists, err := readConfig(paths.ConfigFile.Value)
	if err != nil {
		return Snapshot{}, err
	}
	current, err := validateEditContent(input, paths, raw, exists)
	if err != nil {
		return current, err
	}
	next, changed, err := editContent(raw, key, value)
	if err != nil {
		return current, err
	}
	if !changed {
		return current, nil
	}
	result, err := validateEditContent(input, paths, next, true)
	if err != nil {
		return result, err
	}
	if err := replaceConfig(paths.ConfigFile.Value, next); err != nil {
		return result, err
	}
	return result, nil
}

func protectedKey(key string) bool {
	for part := range strings.SplitSeq(key, ".") {
		if strings.EqualFold(part, "crossSessionInbound") {
			return true
		}
	}
	return false
}

func editableKeys() []string {
	keys := make([]string, 0, len(objectKeys))
	for parent, children := range objectKeys {
		if parent == "" {
			continue
		}
		for _, child := range children {
			key := parent + "." + child
			if _, group := objectKeys[key]; !group {
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

func checkEditKey(key string) error {
	if protectedKey(key) {
		return errOwnerOnly
	}
	keys := editableKeys()
	if !slices.Contains(keys, key) {
		return fmt.Errorf("unknown config key %q. Valid keys: %s", key, strings.Join(keys, ", "))
	}
	return nil
}

func validateEditContent(input Inputs, paths Paths, raw []byte, exists bool) (Snapshot, error) {
	if len(raw) > maxConfigBytes {
		return Snapshot{}, errors.New("config exceeds 65536 bytes. Reduce the config file to at most 65536 bytes")
	}
	// Overrides must not hide an invalid stored value that will fail next time.
	fileOnly := input
	fileOnly.Flags = nil
	fileOnly.LookupEnv = func(string) (string, bool) { return "", false }
	if _, err := loadContent(fileOnly, paths, raw, exists); err != nil {
		return Snapshot{}, err
	}
	return loadContent(input, paths, raw, exists)
}

func editContent(raw []byte, key string, value *string) ([]byte, bool, error) {
	tree := map[string]map[string]json.RawMessage{}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &tree); err != nil {
			return nil, false, err
		}
	}
	parent, leaf, _ := strings.Cut(key, ".")
	if value == nil {
		if _, present := tree[parent][leaf]; !present {
			return raw, false, nil
		}
		delete(tree[parent], leaf)
		if len(tree[parent]) == 0 {
			delete(tree, parent)
		}
	} else {
		encoded, err := encodeEditValue(key, *value)
		if err != nil {
			return nil, false, err
		}
		if tree[parent] == nil {
			tree[parent] = map[string]json.RawMessage{}
		}
		tree[parent][leaf] = encoded
	}
	next, err := json.MarshalIndent(tree, "", "  ")
	return append(next, '\n'), true, err
}

func encodeEditValue(key, value string) (json.RawMessage, error) {
	switch key {
	case "codex.allow_queue_fallback", "messages.max_body_bytes":
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("invalid value for %q. %s", key, typeFix(key))
		}
		return json.RawMessage(value), nil
	default:
		return json.Marshal(value)
	}
}
