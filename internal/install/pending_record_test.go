package install_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// managedFiles are the client files the fixture declares, relative to home.
var managedFiles = []string{".claude.json", ".claude/settings.json", ".codex/config.toml", ".codex/hooks.json", ".cursor/hooks.json", ".cursor/mcp.json"}

// crashHome installs /old/zalki with one user setting, then crashes an
// upgrade to /mid/zalki after n client edits (n < 0: after all of them).
func crashHome(t *testing.T, n int) (home, state string) {
	t.Helper()
	clearConfigOverrides(t)
	home = t.TempDir()
	state = filepath.Join(home, "state")
	writeSettings(t, home, ".claude/settings.json", `{"theme":"user-choice"}`)
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	if _, err := install.CrashAfter(declare, install.Request{Action: "upgrade", Binary: "/mid/zalki", StateDir: state, HomeDir: home, Apply: true}, n); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "installation.pending.json")); err != nil {
		t.Fatal("no pending record before the client writes", err)
	}
	return home, state
}

func checkManaged(t *testing.T, home string, want, gone []string) {
	t.Helper()
	for _, rel := range managedFiles {
		data, _ := os.ReadFile(filepath.Join(home, rel))
		for _, binary := range gone {
			if bytes.Contains(data, []byte(binary)) {
				t.Error(rel, "still has", binary)
			}
		}
		for _, binary := range want {
			if !bytes.Contains(data, []byte(binary)) {
				t.Error(rel, "misses", binary)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(home, ".claude/settings.json")); err != nil || !bytes.Contains(data, []byte(`"theme": "user-choice"`)) {
		t.Error("user setting lost", string(data), err)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "installation.pending.json")); !os.IsNotExist(err) {
		t.Error("pending record left behind", err)
	}
}

func TestCrashedUpgradeRecovers(t *testing.T) {
	for _, n := range []int{0, 1, -1} {
		t.Run("same binary", func(t *testing.T) {
			home, state := crashHome(t, n)
			if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/mid/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
				t.Fatal(n, err)
			}
			checkManaged(t, home, []string{"/mid/zalki"}, []string{"/old/zalki"})
		})
		t.Run("another binary", func(t *testing.T) {
			home, state := crashHome(t, n)
			if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
				t.Fatal(n, err)
			}
			checkManaged(t, home, []string{"/new/zalki"}, []string{"/old/zalki", "/mid/zalki"})
		})
		t.Run("uninstall", func(t *testing.T) {
			home, state := crashHome(t, n)
			if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
				t.Fatal(n, err)
			}
			checkManaged(t, home, nil, []string{"/old/zalki", "/mid/zalki"})
		})
	}
}

func TestCrashedFirstInstallFinishesOrUninstalls(t *testing.T) {
	start := func(t *testing.T) (string, string) {
		t.Helper()
		clearConfigOverrides(t)
		home := t.TempDir()
		state := filepath.Join(home, "state")
		writeSettings(t, home, ".claude/settings.json", `{"theme":"user-choice"}`)
		if _, err := install.CrashAfter(declare, install.Request{Action: "install", Binary: "/old/zalki", StateDir: state, HomeDir: home, Apply: true}, -1); err != nil {
			t.Fatal(err)
		}
		return home, state
	}
	home, state := start(t)
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "/old/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	checkManaged(t, home, nil, []string{"/old/zalki"})

	home, state = start(t)
	if _, err := install.Apply(declare, "/other/zalki", state, home); err == nil || !strings.Contains(err.Error(), "an earlier zalki upgrade did not finish; run zalki upgrade") {
		t.Fatal("install over an unfinished install with other entries", err)
	}
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal("rerun did not finish the install", err)
	}
	checkManaged(t, home, []string{"/old/zalki"}, nil)
	if _, err := os.Stat(filepath.Join(state, "installation.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCrashedUpgradeStillRefusesUserEdits(t *testing.T) {
	home, state := crashHome(t, -1)
	writeSettings(t, home, ".claude.json", `{"mcpServers":{"zalki":{"command":"user-managed"}}}`)
	before := installationSnapshot(t, home)
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err == nil || !strings.Contains(err.Error(), "was edited") {
		t.Fatal("user-edited entry accepted", err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("refused upgrade changed files")
	}
}

func TestInvalidPendingRecordIsRefused(t *testing.T) {
	home, state := crashHome(t, 1)
	pending := filepath.Join(state, "installation.pending.json")
	data, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pending, bytes.Replace(data, []byte(`"schema": 3`), []byte(`"schema": 9`), 1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err == nil || !strings.Contains(err.Error(), "unsupported schema") {
		t.Fatal("invalid pending record accepted", err)
	}
}

func TestFailedWriteKeepsPendingRecordUntilTheNextRun(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(home, ".codex")
	if err := os.Chmod(codex, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(codex, 0700) })
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err == nil {
		t.Fatal("upgrade wrote into a read-only folder")
	}
	if _, err := os.Stat(filepath.Join(state, "installation.pending.json")); err != nil {
		t.Fatal("pending record was not written before the client files", err)
	}
	if err := os.Chmod(codex, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "installation.pending.json")); !os.IsNotExist(err) {
		t.Fatal("pending record left after success", err)
	}
}

func TestInstallationStateMustBePrivate(t *testing.T) {
	setup := func(t *testing.T) (string, string) {
		t.Helper()
		clearConfigOverrides(t)
		home := t.TempDir()
		state := filepath.Join(home, "state")
		if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
			t.Fatal(err)
		}
		return home, state
	}
	upgrade := func(home, state string) error {
		_, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: false})
		return err
	}
	home, state := setup(t)
	if err := os.Chmod(state, 0755); err != nil {
		t.Fatal(err)
	}
	if err := upgrade(home, state); err == nil || err.Error() != "state directory must be a private directory owned by you: "+state {
		t.Fatal("open state directory trusted", err)
	}
	home, state = setup(t)
	link := filepath.Join(home, "state-link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "/old/zalki", StateDir: link, HomeDir: home, Apply: false}); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatal("symlinked state directory trusted", err)
	}
	home, state = setup(t)
	if err := os.Chmod(filepath.Join(state, "installation.json"), 0620); err != nil {
		t.Fatal(err)
	}
	if err := upgrade(home, state); err == nil || !strings.Contains(err.Error(), "installation record must be owned by you and not writable by others") {
		t.Fatal("group-writable record trusted", err)
	}
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "/old/zalki", StateDir: filepath.Join(home, "missing"), HomeDir: home, Apply: true}); err != nil {
		t.Fatal("missing state directory refused", err)
	}
	if _, err := os.Stat(filepath.Join(home, "missing")); !os.IsNotExist(err) {
		t.Fatal("uninstall created the state directory", err)
	}
}

func TestSettingsRewriteKeepsNumbersAndText(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	settings := writeSettings(t, home, ".claude/settings.json", `{"big":9007199254740993,"price":1.50,"text":"a&b<c>"}`)
	cursor := writeSettings(t, home, ".cursor/hooks.json", `{"version":1,"hooks":{}}`)
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	check := func(stage string) {
		t.Helper()
		data, err := os.ReadFile(settings)
		for _, want := range []string{`"big": 9007199254740993`, `"price": 1.50`, `"text": "a&b<c>"`} {
			if err != nil || !bytes.Contains(data, []byte(want)) {
				t.Fatal(stage, "lost", want, string(data), err)
			}
		}
		if _, err := os.Stat(filepath.Join(state, "installation.pending.json")); !os.IsNotExist(err) {
			t.Fatal(stage, "left a pending record", err)
		}
	}
	check("install")
	if data, err := os.ReadFile(cursor); err != nil || !bytes.Contains(data, []byte(`"version": 1`)) {
		t.Fatal("cursor hooks not merged", string(data), err)
	}
	if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	check("upgrade")
	if _, err := install.Manage(declare, install.Request{Action: "uninstall", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	check("uninstall")
}

// TestSecondCrashKeepsTheFirstPendingEntries crashes upgrade B, then crashes
// upgrade C at each point. The next upgrade and uninstall must still remove
// every A, B and C entry.
func TestSecondCrashKeepsTheFirstPendingEntries(t *testing.T) {
	type cut struct {
		first, second int
		action        string
	}
	cuts := make([]cut, 0, 18)
	for _, first := range []int{0, 1, -1} {
		for _, second := range []int{0, 1, -1} {
			cuts = append(cuts, cut{first, second, "upgrade"}, cut{first, second, "uninstall"})
		}
	}
	for _, c := range cuts {
		t.Run(fmt.Sprint(c.first, c.second, c.action), func(t *testing.T) {
			home, state := crashHome(t, c.first)
			if _, err := install.CrashAfter(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}, c.second); err != nil {
				t.Fatal(err)
			}
			if _, err := install.Manage(declare, install.Request{Action: c.action, Binary: "/final/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
				t.Fatal(err)
			}
			checkManaged(t, home, finalBinaries(c.action), []string{"/old/zalki", "/mid/zalki", "/new/zalki"})
		})
	}
}

// finalBinaries is what the managed files hold after the last action.
func finalBinaries(action string) []string {
	if action == "upgrade" {
		return []string{"/final/zalki"}
	}
	return nil
}

// TestFailedSecondUpgradeKeepsTheFirstPendingEntries crashes upgrade B after
// one client file, then lets upgrade C fail and roll back to the mixed state.
func TestFailedSecondUpgradeKeepsTheFirstPendingEntries(t *testing.T) {
	for _, action := range []string{"upgrade", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			home, state := crashHome(t, 1)
			codex := filepath.Join(home, ".codex")
			if err := os.Chmod(codex, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(codex, 0700) })
			if _, err := install.Manage(declare, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err == nil {
				t.Fatal("upgrade wrote into a read-only folder")
			}
			if err := os.Chmod(codex, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := install.Manage(declare, install.Request{Action: action, Binary: "/final/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
				t.Fatal(err)
			}
			checkManaged(t, home, finalBinaries(action), []string{"/old/zalki", "/mid/zalki", "/new/zalki"})
		})
	}
}

func TestCursorHookVersionIsComparedAsANumber(t *testing.T) {
	for body, ok := range map[string]bool{"1": true, "1.0": true, "1e0": true, "2": false, "1.5": false, `"1"`: false} {
		t.Run(body, func(t *testing.T) {
			clearConfigOverrides(t)
			home := t.TempDir()
			hooks := writeSettings(t, home, ".cursor/hooks.json", `{"version":`+body+`,"hooks":{}}`)
			_, err := install.Apply(declare, "/old/zalki", filepath.Join(home, "state"), home)
			if ok != (err == nil) {
				t.Fatal("version", body, err)
			}
			if data, _ := os.ReadFile(hooks); ok && !bytes.Contains(data, []byte(`"version": `+body)) {
				t.Fatal("version spelling not kept", string(data))
			}
		})
	}
}

// TestReformattedNumbersStayZalkiOwned checks that a user's harmless reformat
// of the numbers in managed hook entries (timeout and loop_limit, 5 to 5.0 or
// 5e0) in every hook file does not make the entries look edited: install does
// not add them again, and upgrade and uninstall remove them.
func TestReformattedNumbersStayZalkiOwned(t *testing.T) {
	for _, spelling := range []string{"5.0", "5e0"} {
		for _, action := range []string{"upgrade", "uninstall"} {
			t.Run(spelling+" "+action, func(t *testing.T) { checkReformattedHooks(t, spelling, action) })
		}
	}
}

var managedHookFiles = []string{".claude/settings.json", ".codex/hooks.json", ".cursor/hooks.json"}

func checkReformattedHooks(t *testing.T, spelling, action string) {
	t.Helper()
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	reformatted := reformatHookNumbers(t, home, managedHookFiles, spelling)
	if _, err := install.Apply(declare, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	for _, rel := range managedHookFiles {
		if after, _ := os.ReadFile(filepath.Join(home, rel)); !bytes.Equal(after, reformatted[rel]) {
			t.Fatal(rel, "install added the reformatted entries again", string(after))
		}
	}
	if _, err := install.Manage(declare, install.Request{Action: action, Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range managedHookFiles {
		if after, _ := os.ReadFile(filepath.Join(home, rel)); bytes.Contains(after, []byte("/old/zalki")) {
			t.Fatal(rel, action, "kept the reformatted old hooks", string(after))
		}
	}
}

// reformatHookNumbers rewrites every managed timeout and loop_limit 5 as
// spelling, as a JSON formatter might, and returns the new file contents.
func reformatHookNumbers(t *testing.T, home string, files []string, spelling string) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, rel := range files {
		path := filepath.Join(home, rel)
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(data, []byte(`"timeout": 5`)) {
			t.Fatal(rel, "has no managed timeout", err)
		}
		for _, key := range []string{`"timeout": 5`, `"loop_limit": 5`} {
			data = bytes.ReplaceAll(data, []byte(key), []byte(strings.TrimSuffix(key, "5")+spelling))
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		result[rel] = data
	}
	if !bytes.Contains(result[".cursor/hooks.json"], []byte(`"loop_limit": `+spelling)) {
		t.Fatal("fixture has no Cursor loop_limit")
	}
	return result
}
