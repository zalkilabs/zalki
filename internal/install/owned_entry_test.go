package install_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests use a made-up client, "app", whose hooks file is a map of named
// entries ({"zalki": {...}, "other-tool": {...}}). The client declares the entry
// it owns; the installer does not know the client, the events or the layout of
// the handlers. Every file is under a temporary home.

const appHooksPath = ".app/config/hooks.json"

func appCommand(binary, event string) string {
	return fmt.Sprintf("'%s' 'hook' '--agent' 'app' '--event' '%s'", binary, event)
}

func appEntry(binary string) map[string]any {
	entry := map[string]any{}
	for _, event := range []string{"SessionStart", "PreInvocation", "PostInvocation", "Stop"} {
		entry[event] = []any{map[string]any{"type": "command", "command": appCommand(binary, event), "timeout": 5}}
	}
	return entry
}

func appDeclareFiles(binary, _ string) ([]install.ConfigFile, error) {
	return []install.ConfigFile{{Path: appHooksPath, Draft: "app-hooks.json", JSON: map[string]any{"zalki": appEntry(binary)}, OwnedEntry: "zalki"}}, nil
}

// foreignHooks is what another tool keeps in the same file: nested values,
// unicode, numbers that must keep their digits, and a disabled entry.
const foreignHooks = `{
  "other-tool": {
    "enabled": false,
    "SessionStart": [{"type": "command", "command": "echo \"hi\" && true", "timeout": 30.0}],
    "note": "héllo ✓ <b> & done",
    "numbers": [1.50, 1e3, 12345678901234567890, 0.1, -0, 1E+2, 7],
    "deep": {"a": {"b": [{"c": null, "d": true, "e": []}, {}]}}
  },
  "alpha": [1, "two", {"three": 3.0}],
  "zeta": {}
}`

func appPath(home string) string { return filepath.Join(home, filepath.FromSlash(appHooksPath)) }

func manageApp(t *testing.T, home, action, binary string) {
	t.Helper()
	mustManageWith(t, appDeclareFiles, home, action, binary)
}

// decodeExact reads JSON with numbers as the text the file has.
func decodeExact(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, raw)
	}
	return value
}

func readApp(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(appPath(home))
	if err != nil {
		t.Fatal(err)
	}
	return decodeExact(t, raw)
}

// requireForeignUnchanged checks that every top-level entry of the foreign file is
// still in the file with exactly the same value, numbers by their digits.
func requireForeignUnchanged(t *testing.T, got map[string]any, step string) {
	t.Helper()
	want := decodeExact(t, []byte(foreignHooks))
	for name, value := range want {
		if !reflect.DeepEqual(got[name], value) {
			t.Errorf("%s: the foreign entry %q changed\ngot:  %#v\nwant: %#v", step, name, got[name], value)
		}
	}
}

func TestOwnedEntryInstallWritesOnlyItsEntry(t *testing.T) {
	isolateConfigEnv(t)
	t.Run("into a missing file", func(t *testing.T) {
		home := t.TempDir()
		manageApp(t, home, "install", goldenBinary)
		requireGolden(t, "owned-entry-fresh", snapshotHome(t, home))
	})
	t.Run("next to the entries of other tools", func(t *testing.T) {
		home := t.TempDir()
		seedHome(t, home, map[string]string{appHooksPath: foreignHooks})
		manageApp(t, home, "install", goldenBinary)
		requireGolden(t, "owned-entry-install", snapshotHome(t, home))
		got := readApp(t, home)
		requireForeignUnchanged(t, got, "install")
		if len(got) != len(decodeExact(t, []byte(foreignHooks)))+1 {
			t.Fatalf("top-level entries = %d, want the foreign ones and zalki", len(got))
		}
		if !reflect.DeepEqual(got["zalki"], decodeExact(t, mustMarshal(t, map[string]any{"zalki": appEntry(goldenBinary)}))["zalki"]) {
			t.Fatalf("the zalki entry is not the declared one: %#v", got["zalki"])
		}
	})
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// An upgrade updates the entry in place, keeps the keys the user added to it,
// and a second upgrade to the same binary writes nothing.
func TestOwnedEntryUpgradeUpdatesInPlaceAndKeepsTheUsersKeys(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	seedHome(t, home, map[string]string{appHooksPath: foreignHooks})
	manageApp(t, home, "install", goldenBinary)
	editJSONFile(t, appPath(home), func(v map[string]any) { v["zalki"].(map[string]any)["enabled"] = false })
	manageApp(t, home, "upgrade", goldenNewBinary)
	got := readApp(t, home)
	requireForeignUnchanged(t, got, "upgrade")
	entry := got["zalki"].(map[string]any)
	if entry["enabled"] != false {
		t.Fatalf("the key the user added was not kept: %#v", entry)
	}
	delete(entry, "enabled")
	if !reflect.DeepEqual(entry, decodeExact(t, mustMarshal(t, map[string]any{"zalki": appEntry(goldenNewBinary)}))["zalki"]) {
		t.Fatalf("the zalki entry was not updated: %#v", entry)
	}
	before := snapshotHome(t, home)
	manageApp(t, home, "upgrade", goldenNewBinary)
	if after := snapshotHome(t, home); !equalFiles(before, after) {
		t.Fatal("an upgrade to the same binary changed files")
	}
}

func TestOwnedEntryUpgradeGolden(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	seedHome(t, home, map[string]string{appHooksPath: foreignHooks})
	manageApp(t, home, "install", goldenBinary)
	manageApp(t, home, "upgrade", goldenNewBinary)
	requireGolden(t, "owned-entry-upgrade", snapshotHome(t, home))
}

// An uninstall removes the entry and leaves the entries of other tools with the
// value they had; the keys the user added to ours are named, never their values.
func TestOwnedEntryUninstallLeavesOtherEntriesAndNamesTheUsersKeys(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	seedHome(t, home, map[string]string{appHooksPath: foreignHooks})
	manageApp(t, home, "install", goldenBinary)
	editJSONFile(t, appPath(home), func(v map[string]any) { v["zalki"].(map[string]any)["enabled"] = false })
	path := appPath(home)
	notes := func(apply bool) string {
		request := goldenRequest(home, "uninstall", goldenBinary)
		request.Apply = apply
		report, err := install.Manage(appDeclareFiles, request)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(report.Notes, "\n")
	}
	preview := path + `: entry "zalki" has keys you added (enabled). Uninstall would remove the whole entry with them. Values are not shown. A backup named ` + path + `.zalki-backup-* will keep the old file.`
	if got := notes(false); got != preview {
		t.Fatalf("preview = %q, want %q", got, preview)
	}
	result := path + `: entry "zalki" had keys you added (enabled). Uninstall removed the whole entry with them. Values are not shown. The backup ` + path + `.zalki-backup-* keeps the old file.`
	if got := notes(true); got != result {
		t.Fatalf("result = %q, want %q", got, result)
	}
	got := readApp(t, home)
	requireForeignUnchanged(t, got, "uninstall")
	if _, found := got["zalki"]; found || len(got) != len(decodeExact(t, []byte(foreignHooks))) {
		t.Fatalf("after the uninstall the file has %v, want only the foreign entries", got)
	}
	requireGolden(t, "owned-entry-uninstall", snapshotHome(t, home))
}

func TestOwnedEntryUninstallToleratesAnEntryThatIsGone(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	seedHome(t, home, map[string]string{appHooksPath: foreignHooks})
	manageApp(t, home, "install", goldenBinary)
	editJSONFile(t, appPath(home), func(v map[string]any) { delete(v, "zalki") })
	manageApp(t, home, "uninstall", goldenBinary)
	requireForeignUnchanged(t, readApp(t, home), "uninstall without the entry")
}

// Refusals say what is wrong and the one thing to do, in the installer's wording.
func TestOwnedEntryRefusals(t *testing.T) {
	isolateConfigEnv(t)
	path := appPath
	t.Run("install over an entry zalki did not write", func(t *testing.T) {
		home := t.TempDir()
		seedHome(t, home, map[string]string{appHooksPath: `{"zalki":{"Stop":"mine"}}`})
		requireMessage(t, manageErrorWith(t, appDeclareFiles, home, "install", goldenBinary), path(home)+`: an existing zalki entry differs in key "PostInvocation"; run zalki upgrade, or delete the entry and run zalki install again`)
	})
	t.Run("install over an entry that is not an object", func(t *testing.T) {
		home := t.TempDir()
		seedHome(t, home, map[string]string{appHooksPath: `{"zalki":"x"}`})
		requireMessage(t, manageErrorWith(t, appDeclareFiles, home, "install", goldenBinary), path(home)+`: entry "zalki" is not a JSON object; delete it, then run zalki install again`)
	})
	t.Run("upgrade after the user edited a command", func(t *testing.T) {
		home := t.TempDir()
		manageApp(t, home, "install", goldenBinary)
		editJSONFile(t, path(home), func(v map[string]any) {
			v["zalki"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)["command"] = "mine"
		})
		requireMessage(t, manageErrorWith(t, appDeclareFiles, home, "upgrade", goldenNewBinary), path(home)+`: entry "zalki" key "SessionStart" was edited after Zalki wrote it; restore it or delete that entry, then run zalki upgrade again`)
	})
	t.Run("upgrade after the user added a handler to the entry", func(t *testing.T) {
		home := t.TempDir()
		manageApp(t, home, "install", goldenBinary)
		editJSONFile(t, path(home), func(v map[string]any) {
			entry := v["zalki"].(map[string]any)
			entry["Stop"] = append(entry["Stop"].([]any), map[string]any{"type": "command", "command": "echo mine"})
		})
		requireMessage(t, manageErrorWith(t, appDeclareFiles, home, "upgrade", goldenNewBinary), path(home)+`: entry "zalki" key "Stop" was edited after Zalki wrote it; restore it or delete that entry, then run zalki upgrade again`)
	})
	t.Run("uninstall after the user edited a command", func(t *testing.T) {
		home := t.TempDir()
		manageApp(t, home, "install", goldenBinary)
		editJSONFile(t, path(home), func(v map[string]any) {
			v["zalki"].(map[string]any)["Stop"].([]any)[0].(map[string]any)["command"] = "mine"
		})
		requireMessage(t, manageErrorWith(t, appDeclareFiles, home, "uninstall", goldenBinary), path(home)+`: entry "zalki" key "Stop" was edited after Zalki wrote it; restore it or delete that entry, then run zalki uninstall again`)
	})
	t.Run("uninstall when the entry is not an object", func(t *testing.T) {
		home := t.TempDir()
		manageApp(t, home, "install", goldenBinary)
		editJSONFile(t, path(home), func(v map[string]any) { v["zalki"] = "x" })
		requireMessage(t, manageErrorWith(t, appDeclareFiles, home, "uninstall", goldenBinary), path(home)+`: entry "zalki" was edited; delete it, then run zalki uninstall again`)
	})
	t.Run("a refusal changes no file", func(t *testing.T) {
		home := t.TempDir()
		seedHome(t, home, map[string]string{appHooksPath: foreignHooks})
		manageApp(t, home, "install", goldenBinary)
		editJSONFile(t, path(home), func(v map[string]any) { v["zalki"].(map[string]any)["Stop"] = "mine" })
		before := snapshotHome(t, home)
		manageErrorWith(t, appDeclareFiles, home, "upgrade", goldenNewBinary)
		manageErrorWith(t, appDeclareFiles, home, "uninstall", goldenBinary)
		if after := snapshotHome(t, home); !equalFiles(before, after) {
			t.Fatal("a refused run changed files")
		}
	})
}

func manageErrorWith(t *testing.T, declare install.Declare, home, action, binary string) string {
	t.Helper()
	return manageError(t, declare, home, action, binary)
}

// A recorded patch for such a file must be exactly the declared entry.
func TestOwnedEntryRecordMustHoldExactlyTheEntry(t *testing.T) {
	isolateConfigEnv(t)
	for name, tamper := range map[string]func(patch map[string]any){
		"another key as well":        func(p map[string]any) { p["hooks"] = map[string]any{"Stop": []any{1}} },
		"the entry is not an object": func(p map[string]any) { p["zalki"] = "x" },
		"the entry is empty":         func(p map[string]any) { p["zalki"] = map[string]any{} },
		"another key instead":        func(p map[string]any) { p["mine"] = p["zalki"]; delete(p, "zalki") },
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			manageApp(t, home, "install", goldenBinary)
			record := filepath.Join(home, "state", "installation.json")
			var value map[string]any
			raw, err := os.ReadFile(record)
			if err != nil || json.Unmarshal(raw, &value) != nil {
				t.Fatal(err)
			}
			tamper(value["patches"].(map[string]any)[appHooksPath].(map[string]any))
			if err := os.WriteFile(record, mustMarshal(t, value), 0o600); err != nil {
				t.Fatal(err)
			}
			want := `installation record "` + record + `" is invalid: invalid recorded entry; expected exactly the "zalki" entry. Restore a valid record before retrying.`
			requireMessage(t, manageErrorWith(t, appDeclareFiles, home, "upgrade", goldenNewBinary), want)
		})
	}
}

// A crash between the client edit and the record leaves a pending record; the
// next upgrade finishes, and an uninstall removes the entry the crash left.
func TestOwnedEntryRecoversFromACrashedUpgrade(t *testing.T) {
	isolateConfigEnv(t)
	prepare := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		seedHome(t, home, map[string]string{appHooksPath: foreignHooks})
		manageApp(t, home, "install", goldenBinary)
		if _, err := install.CrashAfter(appDeclareFiles, goldenRequest(home, "upgrade", goldenNewBinary), 1); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(home, "state", "installation.pending.json")); err != nil {
			t.Fatal("the crash left no pending record:", err)
		}
		return home
	}
	t.Run("upgrade again", func(t *testing.T) {
		home := prepare(t)
		manageApp(t, home, "upgrade", goldenNewBinary)
		fresh := t.TempDir()
		seedHome(t, fresh, map[string]string{appHooksPath: foreignHooks})
		manageApp(t, fresh, "install", goldenBinary)
		manageApp(t, fresh, "upgrade", goldenNewBinary)
		if got, want := snapshotHome(t, home), snapshotHome(t, fresh); !equalFiles(got, want) {
			t.Fatalf("after the recovery the files are %v, want those of an upgrade without a crash", sortedNames(got))
		}
	})
	t.Run("uninstall", func(t *testing.T) {
		home := prepare(t)
		manageApp(t, home, "uninstall", goldenNewBinary)
		got := readApp(t, home)
		requireForeignUnchanged(t, got, "uninstall after a crash")
		if _, found := got["zalki"]; found {
			t.Fatal("the entry the crash left was not removed")
		}
		for _, name := range []string{"installation.json", "installation.pending.json"} {
			if _, err := os.Stat(filepath.Join(home, "state", name)); !os.IsNotExist(err) {
				t.Fatalf("%s is still there after the uninstall (%v)", name, err)
			}
		}
	})
}

// Planning and previews never change the maps the adapter declared.
func TestOwnedEntryDeclarationIsNeverChanged(t *testing.T) {
	isolateConfigEnv(t)
	home := t.TempDir()
	seedHome(t, home, map[string]string{appHooksPath: foreignHooks})
	declared, _ := appDeclareFiles(goldenBinary, "")
	want := mustMarshal(t, declared[0].JSON)
	declare := func(string, string) ([]install.ConfigFile, error) { return declared, nil }
	for _, action := range []string{"install", "upgrade", "uninstall"} {
		request := goldenRequest(home, action, goldenBinary)
		request.Apply = false
		if _, err := install.Manage(declare, request); err != nil {
			t.Fatal(err)
		}
		request.Apply = true
		if _, err := install.Manage(declare, request); err != nil {
			t.Fatal(err)
		}
		if got := mustMarshal(t, declared[0].JSON); !bytes.Equal(got, want) {
			t.Fatalf("%s changed the declared patch\ngot:  %s\nwant: %s", action, got, want)
		}
	}
}

// Setup writes the declared entry as a reviewable draft, exactly the patch.
func TestOwnedEntrySetupDraftIsTheDeclaredPatch(t *testing.T) {
	isolateConfigEnv(t)
	home, out := t.TempDir(), filepath.Join(t.TempDir(), "drafts")
	if err := install.WriteSetup(appDeclareFiles, goldenBinary, filepath.Join(home, "state"), out, home, ""); err != nil {
		t.Fatal(err)
	}
	declared, _ := appDeclareFiles(goldenBinary, "")
	want, err := json.MarshalIndent(declared[0].JSON, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "app-hooks.json"))
	if err != nil || string(got) != string(want)+"\n" {
		t.Fatalf("draft = %q (%v), want the declared patch\n%s", got, err, want)
	}
}
