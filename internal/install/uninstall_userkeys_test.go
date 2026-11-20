package install_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests cover uninstall over an entry that holds keys the user added.
// Uninstall removes the whole entry with those keys and their sub-tables, lists
// the key names only, and leaves the old file in the usual backup. It still
// refuses an entry whose owned keys were edited. They use the real Claude,
// Cursor and Codex declarations.

// uuSecret stands for a private value in a key the user added. It must never
// reach a note, an error or any output; only the backup may hold it.
const uuSecret = "sentinel-secret-7f3a91"

const uuCodexKeys = "default_tools_approval_mode = \"approve\"\n\n[mcp_servers.zalki.env]\nTOKEN = \"" + uuSecret + "\"\n"

// uuAddKeys adds user keys with a secret value to the three client files.
func uuAddKeys(t *testing.T, home string) {
	t.Helper()
	for _, rel := range []string{".claude.json", ".cursor/mcp.json"} {
		path := filepath.Join(home, rel)
		doc := ukDoc(t, path)
		entry := ukEntry(t, doc)
		entry["env"] = map[string]any{"TOKEN": uuSecret}
		entry["disabled"] = false
		ukServers(t, doc)["other"] = map[string]any{"command": "keep"}
		if err := writeJSON(path, doc); err != nil {
			t.Fatal(err)
		}
	}
	config := ukCodexPath(home)
	ukWrite(t, config, readFileString(t, config)+uuCodexKeys)
}

func uuUninstall(home, state string, apply bool) (install.Report, error) {
	return install.Manage(declare, install.Request{Action: "uninstall", StateDir: state, HomeDir: home, Apply: apply})
}

// uuText is everything a user could read from a report and an error.
func uuText(t *testing.T, report install.Report, err error) string {
	t.Helper()
	data, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err != nil {
		return string(data) + err.Error()
	}
	return string(data)
}

func uuBackup(t *testing.T, path string) string {
	t.Helper()
	backups, err := filepath.Glob(path + ".zalki-backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("expected exactly one backup", path, backups, err)
	}
	if info, err := os.Stat(backups[0]); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("the backup is not private", info, err)
	}
	return backups[0]
}

func TestUninstallPreviewListsUserKeyNamesOnly(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	uuAddKeys(t, home)
	before := installationSnapshot(t, home)
	report, err := uuUninstall(home, state, false)
	if err != nil || !report.DryRun {
		t.Fatal("the preview failed or was not a preview", err)
	}
	want := map[string]string{
		".claude.json":       "mcpServers.zalki has keys you added (disabled, env). Uninstall would remove the whole entry with them.",
		".codex/config.toml": "[mcp_servers.zalki] has keys you added (default_tools_approval_mode, env). Uninstall would remove the whole table with them.",
		".cursor/mcp.json":   "mcpServers.zalki has keys you added (disabled, env). Uninstall would remove the whole entry with them.",
	}
	if len(report.Notes) != len(want) {
		t.Fatal("expected one note per file", report.Notes)
	}
	for i, rel := range []string{".claude.json", ".codex/config.toml", ".cursor/mcp.json"} {
		path := filepath.Join(home, rel)
		for _, part := range []string{path + ": " + want[rel], "Values are not shown.", "A backup named " + path + ".zalki-backup-* will keep the old file."} {
			if !strings.Contains(report.Notes[i], part) {
				t.Errorf("note %d lacks %q: %s", i, part, report.Notes[i])
			}
		}
	}
	if strings.Contains(uuText(t, report, nil), uuSecret) {
		t.Fatal("a secret value reached the preview")
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("the preview changed files")
	}
}

// uuCheckResultNotes checks the result notes of an apply and that no value is in them.
func uuCheckResultNotes(t *testing.T, report install.Report, err error) {
	t.Helper()
	if len(report.Notes) != 3 {
		t.Fatal("expected one note per file", report.Notes)
	}
	for _, note := range report.Notes {
		if !strings.Contains(note, " had keys you added (") || !strings.Contains(note, "Uninstall removed the whole ") || !strings.Contains(note, "keeps the old file.") {
			t.Error("the result note is wrong", note)
		}
	}
	if strings.Contains(uuText(t, report, err), uuSecret) {
		t.Fatal("a secret value reached the result")
	}
}

func TestUninstallRemovesTheWholeEntryWithUserKeys(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	uuAddKeys(t, home)
	claude, cursor, codex := filepath.Join(home, ".claude.json"), filepath.Join(home, ".cursor", "mcp.json"), ukCodexPath(home)
	old := map[string]string{claude: readFileString(t, claude), cursor: readFileString(t, cursor), codex: readFileString(t, codex)}
	report, err := uuUninstall(home, state, true)
	if err != nil || report.DryRun {
		t.Fatal("uninstall failed", err)
	}
	uuCheckResultNotes(t, report, err)
	for _, path := range []string{claude, cursor} {
		servers := ukServers(t, ukDoc(t, path))
		if _, there := servers["zalki"]; there || servers["other"] == nil {
			t.Fatal("the zalki entry is still there, or another server was lost", path)
		}
	}
	if got := readFileString(t, codex); got != "" {
		t.Fatalf("the codex table was not removed whole: %q", got)
	}
	for path, text := range old {
		if got := readFileString(t, uuBackup(t, path)); got != text || !strings.Contains(got, uuSecret) {
			t.Fatal("the backup does not hold the old file", path)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "installation.json")); !os.IsNotExist(err) {
		t.Fatal("the installation record is still there", err)
	}
	again, err := uuUninstall(home, state, true)
	if err != nil || len(again.Notes) != 0 || len(again.Paths) != 0 {
		t.Fatal("a second uninstall was not quiet", again, err)
	}
}

func TestUninstallKeepsOtherContentInTheCodexFile(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	config := ukCodexPath(home)
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	head := "# my codex settings\nmodel = \"m\"\n\n[mcp_servers.other]\ncommand = \"keep\"\n"
	tail := "\n# notes\n[profiles.work]\nmodel = \"w\"\n"
	ukWrite(t, config, head)
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	installed := readFileString(t, config)
	ukWrite(t, config, installed+"# mine\ndefault_tools_approval_mode = \"approve\"\n[mcp_servers.zalki.env]\nTOKEN = \""+uuSecret+"\"\n"+tail)
	if _, err := uuUninstall(home, state, true); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, config); got != head+tail {
		t.Fatalf("the rest of the file changed\n got %q\nwant %q", got, head+tail)
	}
}

func TestUninstallLeavesAnEmptyServersObjectWhenZalkiWasTheOnlyServer(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	path := filepath.Join(home, ".claude.json")
	doc := ukDoc(t, path)
	ukEntry(t, doc)["env"] = map[string]any{"TOKEN": uuSecret}
	if err := writeJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := uuUninstall(home, state, true); err != nil {
		t.Fatal(err)
	}
	if servers := ukServers(t, ukDoc(t, path)); len(servers) != 0 {
		t.Fatal("mcpServers is not an empty object", servers)
	}
}

func TestUninstallRefusesAnEditedOwnedKeyAndNamesIt(t *testing.T) {
	tests := []struct {
		file, want string
		change     func(t *testing.T, path string)
	}{
		{".claude.json", `mcpServers.zalki key "command" was edited after Zalki wrote it; restore it or delete that entry, then run zalki uninstall again`, func(t *testing.T, path string) {
			t.Helper()
			doc := ukDoc(t, path)
			ukEntry(t, doc)["command"] = "/hand/zalki"
			ukEntry(t, doc)["env"] = map[string]any{"TOKEN": uuSecret}
			if err := writeJSON(path, doc); err != nil {
				t.Fatal(err)
			}
		}},
		{".cursor/mcp.json", `mcpServers.zalki key "args" was edited after Zalki wrote it; restore it or delete that entry, then run zalki uninstall again`, func(t *testing.T, path string) {
			t.Helper()
			doc := ukDoc(t, path)
			delete(ukEntry(t, doc), "args")
			ukEntry(t, doc)["env"] = map[string]any{"TOKEN": uuSecret}
			if err := writeJSON(path, doc); err != nil {
				t.Fatal(err)
			}
		}},
		{".codex/config.toml", `[mcp_servers.zalki] key "command" was edited after Zalki wrote it; restore it or delete that table, then run zalki uninstall again`, func(t *testing.T, path string) {
			t.Helper()
			ukWrite(t, path, strings.Replace(readFileString(t, path), "/old/zalki", "/hand/zalki", 1)+uuCodexKeys)
		}},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			home, state := ukInstalled(t, "/old/zalki")
			path := filepath.Join(home, test.file)
			test.change(t, path)
			before := installationSnapshot(t, home)
			for _, apply := range []bool{false, true} {
				report, err := uuUninstall(home, state, apply)
				if err == nil || !strings.Contains(err.Error(), path+": "+test.want) || strings.Contains(uuText(t, report, err), uuSecret) {
					t.Fatal("an edited owned key was not refused with its name", apply, err)
				}
			}
			if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
				t.Fatal("a refused uninstall changed files")
			}
		})
	}
}

func TestUninstallRefusesCodexLayoutsItCannotRemoveSafely(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	config := ukCodexPath(home)
	ukWrite(t, config, readFileString(t, config)+"[mcp_servers.zalki.env]\nTOKEN = \""+uuSecret+"\"\n[[mcp_servers.zalki]]\ncommand = \"x\"\n")
	before := installationSnapshot(t, home)
	report, err := uuUninstall(home, state, true)
	if err == nil || strings.Contains(uuText(t, report, err), uuSecret) {
		t.Fatal("an unsupported layout was not refused", err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("a refused uninstall changed files")
	}
}

func TestUninstallAfterACrashedUpgradeNamesTheKeys(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	uuAddKeys(t, home)
	if _, err := install.CrashAfter(declare, install.Request{Action: "upgrade", Binary: "/mid/zalki", StateDir: state, HomeDir: home, Apply: true}, -1); err != nil {
		t.Fatal(err)
	}
	report, err := uuUninstall(home, state, true)
	if err != nil || len(report.Notes) != 3 {
		t.Fatal("uninstall over the pending record failed", report.Notes, err)
	}
	if got := readFileString(t, ukCodexPath(home)); got != "" {
		t.Fatalf("the codex table was not removed whole: %q", got)
	}
	for _, name := range []string{"installation.json", "installation.pending.json"} {
		if _, err := os.Stat(filepath.Join(state, name)); !os.IsNotExist(err) {
			t.Fatal(name, "is still there", err)
		}
	}
}

func TestUninstallResultListsAKeyAddedAfterThePreview(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	uuAddKeys(t, home)
	if _, err := uuUninstall(home, state, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude.json")
	doc := ukDoc(t, path)
	ukEntry(t, doc)["late"] = true
	if err := writeJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	report, err := uuUninstall(home, state, true)
	if err != nil || !strings.Contains(report.Notes[0], "(disabled, env, late)") {
		t.Fatal("the result does not list the key added after the preview", report.Notes, err)
	}
}

func TestUninstallWithTheEntryAlreadyGoneRemovesTheRecord(t *testing.T) {
	home, state := ukInstalled(t, "/old/zalki")
	for _, rel := range []string{".claude.json", ".cursor/mcp.json"} {
		path := filepath.Join(home, rel)
		doc := ukDoc(t, path)
		delete(ukServers(t, doc), "zalki")
		if err := writeJSON(path, doc); err != nil {
			t.Fatal(err)
		}
	}
	ukWrite(t, ukCodexPath(home), "")
	report, err := uuUninstall(home, state, true)
	if err != nil || len(report.Notes) != 0 {
		t.Fatal("uninstall over an absent entry failed", report.Notes, err)
	}
	if _, err := os.Stat(filepath.Join(state, "installation.json")); !os.IsNotExist(err) {
		t.Fatal("the installation record is still there", err)
	}
}
