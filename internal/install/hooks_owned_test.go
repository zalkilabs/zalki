package install_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/clientconfig"
	"github.com/zalkilabs/zalki/internal/install"
)

// These tests cover hook entries the user edited. A handler whose command zalki
// recorded is zalki's: upgrade rewrites its command and keeps the user's keys,
// uninstall removes it, and a changed command is refused. They use the real
// Claude, Codex and Cursor declarations.

var hookClients = []struct {
	name, file string
	nested     bool
}{
	{"claude", ".claude/settings.json", true},
	{"codex", ".codex/hooks.json", true},
	{"cursor", ".cursor/hooks.json", false},
}

func hkEvent(t *testing.T, doc map[string]any) string {
	t.Helper()
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok || len(hooks) == 0 {
		t.Fatal("no hooks recorded")
	}
	return slices.Sorted(maps.Keys(hooks))[0]
}

// hkHandler returns the zalki handler of an entry.
func hkHandler(t *testing.T, entry any, nested bool) map[string]any {
	t.Helper()
	object, ok := entry.(map[string]any)
	if !ok {
		t.Fatal("hook entry is not an object")
	}
	if !nested {
		return object
	}
	list, ok := object["hooks"].([]any)
	if !ok || len(list) == 0 {
		t.Fatal("nested entry has no handlers")
	}
	handler, ok := list[0].(map[string]any)
	if !ok {
		t.Fatal("handler is not an object")
	}
	return handler
}

func hkCommand(handler map[string]any) string {
	command, _ := handler["command"].(string)
	return command
}

// hkEdit gives the first event's zalki hook a user key and another timeout.
func hkEdit(t *testing.T, home, file string, nested bool) (path, event string) {
	t.Helper()
	path = filepath.Join(home, file)
	doc := ukDoc(t, path)
	event = hkEvent(t, doc)
	entry := ukHookEntries(t, doc, event)[0].(map[string]any)
	entry["user_note"] = true
	hkHandler(t, entry, nested)["timeout"] = 30
	if err := writeJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	return path, event
}

func hkSetHandlers(t *testing.T, path, event string, change func(entries []any) []any) {
	t.Helper()
	doc := ukDoc(t, path)
	hooks := doc["hooks"].(map[string]any)
	hooks[event] = change(ukHookEntries(t, doc, event))
	if err := writeJSON(path, doc); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeRewritesAnEditedZalkiHookInEveryClient(t *testing.T) {
	for _, client := range hookClients {
		t.Run(client.name, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			path, event := hkEdit(t, home, client.file, client.nested)
			if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
				t.Fatal(err)
			}
			doc := ukDoc(t, path)
			checkEditedHookRewritten(t, doc, event, client.nested)
			checkNoStaleHooks(t, doc, client.nested)
			if client.name == "cursor" && !reflect.DeepEqual(doc["version"], float64(1)) {
				t.Fatal("the Cursor schema version changed", doc["version"])
			}
		})
	}
}

func checkEditedHookRewritten(t *testing.T, doc map[string]any, event string, nested bool) {
	t.Helper()
	entries := ukHookEntries(t, doc, event)
	if len(entries) != 1 {
		t.Fatal("expected one zalki hook for the event", len(entries))
	}
	entry := entries[0].(map[string]any)
	handler := hkHandler(t, entry, nested)
	if command := hkCommand(handler); !strings.Contains(command, "/new/zalki") || strings.Contains(command, "/old/zalki") {
		t.Fatal("the command was not rewritten", command)
	}
	if entry["user_note"] != true || !reflect.DeepEqual(handler["timeout"], float64(30)) {
		t.Fatal("the user's edits were lost", entry)
	}
}

func checkNoStaleHooks(t *testing.T, doc map[string]any, nested bool) {
	t.Helper()
	for event := range doc["hooks"].(map[string]any) {
		list := ukHookEntries(t, doc, event)
		if len(list) != 1 || strings.Contains(hkCommand(hkHandler(t, list[0], nested)), "/old/zalki") {
			t.Fatal("an event kept an old or second hook", event)
		}
	}
}

func TestUpgradeKeepsTheUsersOwnHookAndRewritesEveryRecognizedHandler(t *testing.T) {
	for _, client := range hookClients {
		t.Run(client.name, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			path := filepath.Join(home, client.file)
			event := hkEvent(t, ukDoc(t, path))
			hkSetHandlers(t, path, event, func(entries []any) []any { return hkAddUserHandler(entries, client.nested) })
			if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
				t.Fatal(err)
			}
			mine, zalki := hkCountCommands(t, hkCommands(ukHookEntries(t, ukDoc(t, path), event), client.nested))
			wantZalki := 1
			if client.nested {
				wantZalki = 2 // the user's copy of the handler is rewritten too
			}
			if mine != 1 || zalki != wantZalki {
				t.Fatal("unexpected commands", mine, zalki)
			}
		})
	}
}

// hkAddUserHandler adds the user's own hook and, in a nested entry, a copy of the zalki handler.
func hkAddUserHandler(entries []any, nested bool) []any {
	if !nested {
		return append(entries, map[string]any{"command": "echo mine"})
	}
	entry := entries[0].(map[string]any)
	list := entry["hooks"].([]any)
	twin := maps.Clone(list[0].(map[string]any))
	entry["hooks"] = append(list, twin, map[string]any{"command": "echo mine", "type": "command"})
	return entries
}

func hkCommands(entries []any, nested bool) []string {
	var commands []string
	for _, entry := range entries {
		if !nested {
			commands = append(commands, hkCommand(entry.(map[string]any)))
			continue
		}
		for _, item := range entry.(map[string]any)["hooks"].([]any) {
			commands = append(commands, hkCommand(item.(map[string]any)))
		}
	}
	return commands
}

// hkCountCommands counts the user's commands and the rewritten zalki commands.
func hkCountCommands(t *testing.T, commands []string) (mine, zalki int) {
	t.Helper()
	for _, command := range commands {
		switch {
		case command == "echo mine":
			mine++
		case strings.Contains(command, "/new/zalki"):
			zalki++
		default:
			t.Fatal("a stale or unknown command is left", command)
		}
	}
	return mine, zalki
}

var hkChanges = map[string]func(command string) string{
	"unquoted":       func(c string) string { return strings.ReplaceAll(c, "'", "") },
	"redirected":     func(c string) string { return c + " >/dev/null 2>&1" },
	"env prefix":     func(c string) string { return "ZALKI_X=1 " + c },
	"other binary":   func(c string) string { return strings.ReplaceAll(c, "/old/zalki", "/other/zalki") },
	"or true suffix": func(c string) string { return c + " || true" },
	"equals form":    func(c string) string { return strings.Replace(c, "'--agent' '", "'--agent=", 1) },
}

func TestUpgradeAndUninstallRefuseAChangedZalkiHookCommand(t *testing.T) {
	for _, client := range hookClients {
		for name, change := range hkChanges {
			t.Run(client.name+"/"+name, func(t *testing.T) {
				checkChangedCommandRefused(t, client.file, client.nested, change)
			})
		}
	}
}

func checkChangedCommandRefused(t *testing.T, file string, nested bool, change func(string) string) {
	t.Helper()
	home, state := ukInstalled(t, "/old/zalki")
	path := filepath.Join(home, file)
	doc := ukDoc(t, path)
	event := hkEvent(t, doc)
	handler := hkHandler(t, ukHookEntries(t, doc, event)[0], nested)
	handler["command"] = change(hkCommand(handler))
	if err := writeJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	before := installationSnapshot(t, home)
	err := ukUpgrade(home, state, "/new/zalki")
	want := "hooks." + event + " has a zalki hook command you changed"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "run zalki upgrade again") || !strings.Contains(err.Error(), path) {
		t.Fatal("upgrade did not refuse the changed command", err)
	}
	_, err = install.Manage(declare, install.Request{Action: "uninstall", StateDir: state, HomeDir: home, Apply: true})
	if err == nil || !strings.Contains(err.Error(), "run zalki uninstall again") {
		t.Fatal("uninstall did not refuse the changed command", err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("a refused command changed files")
	}
}

func TestUpgradeWithAnExactAndAnEditedRecordedHookKeepsOne(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	path := filepath.Join(home, ".claude", "settings.json")
	doc := ukDoc(t, path)
	event := hkEvent(t, doc)
	exact := marshalString(t, ukHookEntries(t, doc, event)[0])
	hkEdit(t, home, ".claude/settings.json", true)
	hkSetHandlers(t, path, event, func(entries []any) []any {
		var copyOfExact map[string]any
		if err := json.Unmarshal([]byte(exact), &copyOfExact); err != nil {
			t.Fatal(err)
		}
		return append(entries, copyOfExact)
	})
	if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
		t.Fatal(err)
	}
	entries := ukHookEntries(t, ukDoc(t, path), event)
	if len(entries) != 1 || entries[0].(map[string]any)["user_note"] != true {
		t.Fatal("expected the edited entry only", marshalString(t, entries))
	}
}

func TestASecondUpgradeAfterAdoptingAnEditedHookWritesNothing(t *testing.T) {
	for _, client := range hookClients {
		t.Run(client.name, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			hkEdit(t, home, client.file, client.nested)
			if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
				t.Fatal(err)
			}
			edits, err := install.PlannedEdits(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home})
			if err != nil || edits != 0 {
				t.Fatal("a second upgrade would still write", edits, err)
			}
		})
	}
}

func TestUninstallPreviewAndResultNameTheChangedHookKeysOnly(t *testing.T) {
	for _, client := range hookClients {
		t.Run(client.name, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			path, event := hkEdit(t, home, client.file, client.nested)
			doc := ukDoc(t, path)
			hkHandler(t, ukHookEntries(t, doc, event)[0], client.nested)["statusMessage"] = "SECRET-VALUE-123"
			if err := writeJSON(path, doc); err != nil {
				t.Fatal(err)
			}
			request := install.Request{Action: "uninstall", StateDir: state, HomeDir: home}
			preview := hkOneNote(t, request, "would remove")
			request.Apply = true
			result := hkOneNote(t, request, "removed it")
			for _, note := range []string{preview, result} {
				checkHookNote(t, note, path, event)
			}
		})
	}
}

// hkOneNote runs a request and returns its only note, which must contain tense.
func hkOneNote(t *testing.T, request install.Request, tense string) string {
	t.Helper()
	report, err := install.Manage(declare, request)
	if err != nil || len(report.Notes) != 1 || !strings.Contains(report.Notes[0], tense) {
		t.Fatal("unexpected report", report, err)
	}
	return report.Notes[0]
}

func checkHookNote(t *testing.T, note, path, event string) {
	t.Helper()
	if !strings.HasPrefix(note, path+": the zalki hook for "+event) || strings.Contains(note, "SECRET") || strings.Contains(note, "/old/zalki") {
		t.Fatal("unexpected note", note)
	}
	for _, key := range []string{"statusMessage", "timeout", "user_note"} {
		if !strings.Contains(note, key) {
			t.Fatal("the note lacks a changed key", key, note)
		}
	}
}

func TestUninstallWithAPendingRecordRemovesAnEditedHookOfBothRecords(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	request := install.Request{Action: "upgrade", Binary: "/mid/zalki", StateDir: state, HomeDir: home, Apply: true}
	total, err := install.PlannedEdits(declare, request)
	if err != nil || total < 2 {
		t.Fatal("the crash setup failed", total, err)
	}
	if _, err := install.CrashAfter(declare, request, total-1); err != nil {
		t.Fatal(err)
	}
	path, event := hkEdit(t, home, ".claude/settings.json", true)
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if entries := ukHookEntries(t, ukDoc(t, path), event); len(entries) != 0 {
		t.Fatal("the edited hook was not removed", marshalString(t, entries))
	}
}

func TestUpgradeKeepsAnUnrecognizedZalkiSiblingNextToARecognizedHandler(t *testing.T) {
	// A recognized handler plus an unrecognized zalki-kind sibling is not refused: the
	// recognized one is rewritten and the sibling stays as the user's own.
	home, state := ukInstalled(t, "/old/zalki")
	path := filepath.Join(home, ".claude", "settings.json")
	event := hkEvent(t, ukDoc(t, path))
	hkSetHandlers(t, path, event, func(entries []any) []any {
		sibling := map[string]any{"command": "'/other/zalki' 'hook' '--agent' 'claude'", "type": "command"}
		entry := entries[0].(map[string]any)
		entry["hooks"] = append(entry["hooks"].([]any), sibling)
		return entries
	})
	if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
		t.Fatal(err)
	}
	joined := marshalString(t, ukHookEntries(t, ukDoc(t, path), event))
	if strings.Count(joined, "/new/zalki") != 1 || strings.Count(joined, "/other/zalki") != 1 || strings.Contains(joined, "/old/zalki") {
		t.Fatal("unexpected hooks", joined)
	}
}

func TestInstallOverAnEditedZalkiHookAddsNoSecondHook(t *testing.T) {
	for _, client := range hookClients {
		t.Run(client.name, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			path, event := hkEdit(t, home, client.file, client.nested)
			if _, err := install.Manage(declare, install.Request{Action: "install", Binary: "/old/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
				t.Fatal(err)
			}
			entries := ukHookEntries(t, ukDoc(t, path), event)
			if len(entries) != 1 || entries[0].(map[string]any)["user_note"] != true {
				t.Fatal("install added a second hook or lost the edit", entries)
			}
		})
	}
}

func TestInstallRefusesADifferentZalkiHookCommandWithInstallWording(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	other := clientconfig.HookConfig(map[string]string{"Stop": "Stop"}, clientconfig.HookCommand("/old/zalki", []string{"--state", "s"}, "claude"), false)
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, other); err != nil {
		t.Fatal(err)
	}
	_, err := install.Manage(declare, install.Request{Action: "install", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true})
	if err == nil || !strings.Contains(err.Error(), "hooks.Stop has a zalki hook command you changed") || !strings.Contains(err.Error(), "run zalki install again") {
		t.Fatal("install did not refuse another zalki hook command", err)
	}
}

func TestUpgradeLeavesOtherKindsAndNonStringCommandsAlone(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	path := filepath.Join(home, ".claude", "settings.json")
	event := hkEvent(t, ukDoc(t, path))
	hkSetHandlers(t, path, event, func(entries []any) []any {
		return append(entries,
			map[string]any{"hooks": []any{map[string]any{"command": "'/x/zalki' 'hook' '--agent' 'codex'", "type": "command"}}},
			map[string]any{"hooks": []any{map[string]any{"command": 5, "type": "command"}}})
	})
	if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
		t.Fatal(err)
	}
	joined := marshalString(t, ukHookEntries(t, ukDoc(t, path), event))
	if strings.Count(joined, "'--agent' 'codex'") != 1 || !strings.Contains(joined, `"command":5`) || strings.Count(joined, "/new/zalki") != 1 || strings.Contains(joined, "/old/zalki") {
		t.Fatal("unexpected hook entries", joined)
	}
}

func TestUpgradeAfterACrashRecognizesTheHookOfTheUnfinishedRecord(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	request := install.Request{Action: "upgrade", Binary: "/mid/zalki", StateDir: state, HomeDir: home, Apply: true}
	total, err := install.PlannedEdits(declare, request)
	if err != nil || total < 2 {
		t.Fatal("the crash setup failed", total, err)
	}
	edits, err := install.CrashAfter(declare, request, total-1)
	if err != nil || edits == 0 {
		t.Fatal("the crash setup failed", edits, err)
	}
	path, event := hkEdit(t, home, ".claude/settings.json", true)
	if err := ukUpgrade(home, state, "/new/zalki"); err != nil {
		t.Fatal(err)
	}
	entries := ukHookEntries(t, ukDoc(t, path), event)
	joined := marshalString(t, entries)
	if len(entries) != 1 || strings.Contains(joined, "/mid/zalki") || strings.Contains(joined, "/old/zalki") || !strings.Contains(joined, "user_note") {
		t.Fatal("the unfinished record's hook was not rewritten", joined)
	}
}

func TestUninstallRemovesAnEditedZalkiHookInEveryClient(t *testing.T) {
	for _, client := range hookClients {
		t.Run(client.name, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			path, event := hkEdit(t, home, client.file, client.nested)
			if _, err := install.Manage(declare, install.Request{Action: "uninstall", StateDir: state, HomeDir: home, Apply: true}); err != nil {
				t.Fatal(err)
			}
			if entries := ukHookEntries(t, ukDoc(t, path), event); len(entries) != 0 {
				t.Fatal("the edited zalki hook was not removed", entries)
			}
			if backups, err := filepath.Glob(path + ".zalki-backup-*"); err != nil || len(backups) != 1 {
				t.Fatal("the old file was not backed up", backups, err)
			}
		})
	}
}

func hookPatch(flat bool) map[string]any {
	return clientconfig.HookConfig(map[string]string{"Stop": "Stop"}, hkCommandText, flat)
}

func hookDoc(entries ...any) map[string]any {
	return map[string]any{"hooks": map[string]any{"Stop": entries}}
}

func hookStop(doc map[string]any) []any { return doc["hooks"].(map[string]any)["Stop"].([]any) }

const hkCommandText = "'/secret/home/zalki' '--config' 'c' '--state' 's' 'hook' '--agent' 'claude'"

func hkZalkiHandler() map[string]any {
	return map[string]any{"command": hkCommandText, "timeout": 5, "type": "command"}
}

func TestRemoveRecordedHooksRemovesAnExactEntryWithoutANote(t *testing.T) {
	recorded := hookPatch(false)
	doc := hookDoc(hookStop(recorded)[0])
	notes, err := install.RemoveRecordedHooks(doc, []map[string]any{recorded}, true)
	if err != nil || len(notes) != 0 || len(hookStop(doc)) != 0 {
		t.Fatal("unexpected result", notes, err, doc)
	}
}

func TestRemoveRecordedHooksKeepsTheUsersHandlerAndNamesChangedKeys(t *testing.T) {
	edited := hkZalkiHandler()
	edited["timeout"] = 30
	edited["statusMessage"] = "SECRET-VALUE-123"
	doc := hookDoc(map[string]any{"matcher": "SECRET-MATCHER", "hooks": []any{edited, map[string]any{"command": "echo mine"}}})
	notes, err := install.RemoveRecordedHooks(doc, []map[string]any{hookPatch(false)}, true)
	left := hookStop(doc)
	if err != nil || len(left) != 1 || len(left[0].(map[string]any)["hooks"].([]any)) != 1 {
		t.Fatal("unexpected result", err, doc)
	}
	want := "PATH: the zalki hook for Stop had keys you added or changed (statusMessage, timeout). Uninstall removed it with them. Values are not shown. The backup PATH.zalki-backup-* keeps the old file."
	if !reflect.DeepEqual(notes, []string{want}) {
		t.Fatal("unexpected notes", notes)
	}
}

func TestRemoveRecordedHooksDropsAnEmptiedEntryAndNeverPrintsValues(t *testing.T) {
	doc := hookDoc(map[string]any{"matcher": "SECRET-MATCHER", "hooks": []any{hkZalkiHandler()}})
	notes, err := install.RemoveRecordedHooks(doc, []map[string]any{hookPatch(false)}, true)
	if err != nil || len(hookStop(doc)) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "matcher") {
		t.Fatal("unexpected result", notes, err, doc)
	}
	if joined := strings.Join(notes, " "); strings.Contains(joined, "SECRET") || strings.Contains(joined, "/secret/home") {
		t.Fatal("a note printed a value or the command", joined)
	}
}

func TestRemoveRecordedHooksRemovesAFlatEditedEntry(t *testing.T) {
	entry := map[string]any{"command": hkCommandText, "timeout": 5, "user_note": "SECRET-VALUE-123"}
	doc := hookDoc(entry)
	notes, err := install.RemoveRecordedHooks(doc, []map[string]any{hookPatch(true)}, true)
	if err != nil || len(hookStop(doc)) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "user_note") || strings.Contains(notes[0], "SECRET") {
		t.Fatal("unexpected result", notes, err, doc)
	}
}

func TestRemoveRecordedHooksForUpgradeKeepsAnEditedHandlerForTheMerge(t *testing.T) {
	edited := hkZalkiHandler()
	edited["timeout"] = 30
	doc := hookDoc(map[string]any{"hooks": []any{edited}})
	notes, err := install.RemoveRecordedHooks(doc, []map[string]any{hookPatch(false)}, false)
	if err != nil || len(notes) != 0 || len(hookStop(doc)) != 1 {
		t.Fatal("unexpected result", notes, err, doc)
	}
}

func TestRemoveRecordedHooksRefusesAChangedCommandAndBadShapes(t *testing.T) {
	recorded := []map[string]any{hookPatch(false)}
	changed := hookDoc(map[string]any{"hooks": []any{map[string]any{"command": "wrapper " + hkCommandText + " >/dev/null"}}})
	if _, err := install.RemoveRecordedHooks(changed, recorded, true); err == nil || !strings.Contains(err.Error(), "hooks.Stop has a zalki hook command you changed") || strings.Contains(err.Error(), "/secret/home") {
		t.Fatal("unexpected error", err)
	}
	if _, err := install.RemoveRecordedHooks(map[string]any{"hooks": []any{}}, recorded, true); err == nil || err.Error() != "hooks is not an object" {
		t.Fatal("unexpected error", err)
	}
	if _, err := install.RemoveRecordedHooks(map[string]any{"hooks": map[string]any{"Stop": "x"}}, recorded, true); err == nil || err.Error() != "hook event is not an array" {
		t.Fatal("unexpected error", err)
	}
}

func marshalString(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRemoveRecordedHooksCapsAndQuotesTheKeyNames(t *testing.T) {
	edited := hkZalkiHandler()
	for i := 0; i < 25; i++ {
		edited[fmt.Sprintf("key%02d", i)] = true
	}
	edited["bad\x1b[31mname"] = true
	doc := hookDoc(map[string]any{"hooks": []any{edited}})
	notes, err := install.RemoveRecordedHooks(doc, []map[string]any{hookPatch(false)}, true)
	if err != nil || len(notes) != 1 || strings.Contains(notes[0], "\x1b") || !strings.Contains(notes[0], ", and 6 more)") {
		t.Fatal("unexpected note", notes, err)
	}
}
