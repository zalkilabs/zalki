package install

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// A hook entry of a client settings file holds handlers. Claude Code and Codex
// nest them: {"hooks":[{"command":...}], ...}. Cursor writes a flat entry that
// is its own handler. The owned part of a hook is the handler's command: a
// handler whose command was recorded is zalki's, whatever else the user added
// or changed next to it. Upgrade rewrites the command and keeps the rest.
// Uninstall removes the handler and names the keys the user had changed. Both
// refuse a handler that runs "hook --agent <kind>" of this client but whose
// command is not one zalki recorded or wants now.

// recordedHook is a handler zalki recorded and the entry that held it.
type recordedHook struct{ entry, handler map[string]any }

// hookHandlers returns the handler objects of one hook entry.
func hookHandlers(entry any) []map[string]any {
	object, ok := entry.(map[string]any)
	if !ok {
		return nil
	}
	list, nested := object["hooks"].([]any)
	if !nested {
		return []map[string]any{object}
	}
	handlers := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if handler, isObject := item.(map[string]any); isObject {
			handlers = append(handlers, handler)
		}
	}
	return handlers
}

func handlerCommand(handler map[string]any) string {
	command, _ := handler["command"].(string)
	return command
}

// hookKind returns the kind a hook command runs ("claude" in "... hook --agent
// claude" or "... hook --agent=claude"), or "". It reads words, so a user who
// drops the quotes or adds a redirect or an environment prefix does not hide a zalki hook.
func hookKind(command string) string {
	words := strings.Fields(strings.NewReplacer("'", " ", `"`, " ").Replace(command))
	for i := len(words) - 2; i >= 0; i-- {
		if words[i] != "hook" {
			continue
		}
		if kind, ok := strings.CutPrefix(words[i+1], "--agent="); ok {
			return kind
		}
		if words[i+1] == "--agent" && i+2 < len(words) {
			return words[i+2]
		}
	}
	return ""
}

// recordedAdditions lists the entries the recorded patches added for one event.
func recordedAdditions(patches []map[string]any, event string) []any {
	additions := make([]any, 0, len(patches))
	for _, patch := range patches {
		hooks, _ := patch["hooks"].(map[string]any)
		list, _ := hooks[event].([]any)
		additions = append(additions, list...)
	}
	return additions
}

// recordedHooks maps each recorded handler command of one event to its handler and entry.
func recordedHooks(patches []map[string]any, event string) map[string]recordedHook {
	recorded := map[string]recordedHook{}
	for _, addition := range recordedAdditions(patches, event) {
		entry, _ := addition.(map[string]any)
		for _, handler := range hookHandlers(addition) {
			if command := handlerCommand(handler); command != "" {
				recorded[command] = recordedHook{entry: entry, handler: handler}
			}
		}
	}
	return recorded
}

// changedHookCommandError is the refusal for a handler that runs this client's
// zalki hook but whose command zalki never recorded: the user changed it.
func changedHookCommandError(event, action string) error {
	return fmt.Errorf("hooks.%s has a zalki hook command you changed; restore it or delete that hook, then run zalki %s again", event, action)
}

// hasChangedHookCommand reports whether a handler of entries runs the hook of
// one of kinds but is not recognized.
func hasChangedHookCommand(entries []any, kinds map[string]bool, recognized func(string) bool) bool {
	for _, entry := range entries {
		for _, handler := range hookHandlers(entry) {
			command := handlerCommand(handler)
			if kind := hookKind(command); kind != "" && kinds[kind] && !recognized(command) {
				return true
			}
		}
	}
	return false
}

// addWantedHook adds the wanted entry of one event to entries. Every handler
// recognized as zalki's (a recorded command, or the wanted command itself) gets
// the wanted command and keeps everything else; no second hook is added. A
// handler that runs this client's hook but is not recognized is refused.
func addWantedHook(entries []any, event string, wanted any, recorded map[string]recordedHook, action string) ([]any, error) {
	var command string
	if handlers := hookHandlers(wanted); len(handlers) > 0 {
		command = handlerCommand(handlers[0])
	}
	recognized := func(c string) bool { _, ok := recorded[c]; return c != "" && (ok || c == command) }
	adopted := false
	for _, entry := range entries {
		for _, handler := range hookHandlers(entry) {
			if recognized(handlerCommand(handler)) {
				handler["command"] = command
				adopted = true
			}
		}
	}
	if adopted {
		return entries, nil
	}
	if kind := hookKind(command); kind != "" && hasChangedHookCommand(entries, map[string]bool{kind: true}, recognized) {
		return nil, changedHookCommandError(event, action)
	}
	return append(entries, wanted), nil
}

// changedKeys names the keys of have that recorded lacks or has with another value.
func changedKeys(have, recorded map[string]any) []string {
	var names []string
	for _, key := range slices.Sorted(maps.Keys(have)) {
		if value, ok := recorded[key]; !ok || !equalJSON(have[key], value) {
			names = append(names, key)
		}
	}
	return names
}

func hooksEventNames(patches []map[string]any) []string {
	events := map[string]bool{}
	for _, patch := range patches {
		hooks, _ := patch["hooks"].(map[string]any)
		for event := range hooks {
			events[event] = true
		}
	}
	return slices.Sorted(maps.Keys(events))
}

// hookRemoval says which keys the user had added or changed on the recorded
// hooks of one event that an uninstall removed. Only key names are kept.
type hookRemoval struct {
	event string
	keys  []string
}

// removeRecordedHooks takes the recorded hooks out of the settings. An entry equal
// to a recorded one is always removed. With uninstall, a handler that still has a
// recorded command is removed too, and the keys the user had changed are returned
// (names only, never values). Every other entry stays.
func removeRecordedHooks(current map[string]any, patches []map[string]any, uninstall bool) ([]hookRemoval, error) {
	events := hooksEventNames(patches)
	if len(events) == 0 || current["hooks"] == nil {
		return nil, nil
	}
	hooks, ok := current["hooks"].(map[string]any)
	if !ok {
		return nil, errors.New("hooks is not an object")
	}
	var removed []hookRemoval
	for _, event := range events {
		keys, err := removeRecordedHooksOf(hooks, event, patches, uninstall)
		if err != nil {
			return nil, err
		}
		if len(keys) > 0 {
			removed = append(removed, hookRemoval{event: event, keys: keys})
		}
	}
	return removed, nil
}

// removeRecordedHooksOf does removeRecordedHooks for one event and returns the changed key names.
func removeRecordedHooksOf(hooks map[string]any, event string, patches []map[string]any, uninstall bool) ([]string, error) {
	if hooks[event] == nil {
		return nil, nil
	}
	entries, isList := hooks[event].([]any)
	if !isList {
		return nil, errors.New("hook event is not an array")
	}
	additions := recordedAdditions(patches, event)
	kept := []any{}
	for _, entry := range entries {
		if !containsJSON(additions, entry) {
			kept = append(kept, entry)
		}
	}
	var keys []string
	if uninstall {
		var err error
		if kept, keys, err = removeEditedHooks(kept, event, recordedHooks(patches, event)); err != nil {
			return nil, err
		}
	}
	hooks[event] = kept
	return keys, nil
}

// removeEditedHooks removes the recorded handlers that are left in entries and
// returns the key names the user had changed on them.
func removeEditedHooks(entries []any, event string, recorded map[string]recordedHook) ([]any, []string, error) {
	kinds := map[string]bool{}
	for command := range recorded {
		if kind := hookKind(command); kind != "" {
			kinds[kind] = true
		}
	}
	known := func(c string) bool { _, ok := recorded[c]; return ok }
	if hasChangedHookCommand(entries, kinds, known) {
		return nil, nil, changedHookCommandError(event, "uninstall")
	}
	kept := make([]any, 0, len(entries))
	var names []string
	for _, entry := range entries {
		stays, removed := removeHandlers(entry, recorded)
		names = append(names, removed...)
		if stays {
			kept = append(kept, entry)
		}
	}
	return kept, slices.Compact(slices.Sorted(slices.Values(names))), nil
}

// hookNotes tells the owner which keys went with the removed hooks. It names keys
// only. The backup stamp is random, so the note names the backup with a star.
func hookNotes(path string, removed []hookRemoval) []editNote {
	notes := make([]editNote, 0, len(removed))
	for _, hook := range removed {
		list := listUserKeys(hook.keys)
		notes = append(notes, editNote{
			preview: fmt.Sprintf("%s: the zalki hook for %s has keys you added or changed (%s). Uninstall would remove it with them. Values are not shown. A backup named %s.zalki-backup-* will keep the old file.", path, hook.event, list, path),
			result:  fmt.Sprintf("%s: the zalki hook for %s had keys you added or changed (%s). Uninstall removed it with them. Values are not shown. The backup %s.zalki-backup-* keeps the old file.", path, hook.event, list, path),
		})
	}
	return notes
}

// removeHandlers removes the recorded handlers of one entry. It reports whether
// the entry stays (a nested entry with other handlers) and the key names the
// user had changed on what was removed.
func removeHandlers(entry any, recorded map[string]recordedHook) (stays bool, names []string) {
	object, isObject := entry.(map[string]any)
	if !isObject {
		return true, nil
	}
	list, nested := object["hooks"].([]any)
	if !nested {
		if hook, ok := recorded[handlerCommand(object)]; ok {
			return false, changedKeys(object, hook.handler)
		}
		return true, nil
	}
	remaining := make([]any, 0, len(list))
	var last recordedHook
	removed := false
	for _, item := range list {
		handler, isHandler := item.(map[string]any)
		hook, ok := recorded[handlerCommand(handler)]
		if !isHandler || !ok {
			remaining = append(remaining, item)
			continue
		}
		removed, last = true, hook
		names = append(names, changedKeys(handler, hook.handler)...)
	}
	if !removed {
		return true, nil
	}
	if len(remaining) > 0 {
		object["hooks"] = remaining
		return true, names
	}
	for _, name := range changedKeys(object, last.entry) {
		if name != "hooks" {
			names = append(names, name)
		}
	}
	return false, names
}
