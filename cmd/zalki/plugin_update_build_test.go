package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginUpdateDropsExtendedGrants(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	manifest := strings.ReplaceAll(strings.ReplaceAll(nativeInstallManifest, `"send":false`, `"send":true`), `"read":false`, `"read":true`)
	folder := writeInstallFolder(t, manifest, map[string]string{"bin/plugin": "#!/bin/sh\nexit 0\n"})
	output, err := pluginInstallLocal(t, store.Dir, installPlainLines, "#1\n#1\ny\n", folder)
	if err != nil {
		t.Fatal(output, err)
	}
	old := readInstallStatus(t, store.Dir, "demo")
	if len(old.Access.Send) != 1 || len(old.Access.Read) != 1 {
		t.Fatal("extended fixture not granted")
	}
	mustWriteInstallManifest(t, folder, []byte(nativeInstallManifest))
	output, err = pluginUpdateLocal(t, store.Dir, installPlainLines, "y\n", "demo")
	if err != nil {
		t.Fatal(output, err)
	}
	now := readInstallStatus(t, store.Dir, "demo")
	if len(now.Access.Send)+len(now.Access.Read) != 0 || now.Revision == old.Revision || now.Epoch == old.Epoch || strings.Count(output, "(removed)") != 2 {
		t.Fatal("update kept removed extended grants or hid their removal", output)
	}
}

func TestPluginUpdateBuildFailureKeepsOldAndScrubsEnvironment(t *testing.T) {
	state, _, old := installedUpdateFolder(t, "demo")
	t.Setenv("ZAL15_SECRET_CREDENTIAL", "private-sentinel")
	folder := fakeGoFolder(t, `if [ "${ZAL15_SECRET_CREDENTIAL+x}" = x ]; then echo ENV_LEAK; fi
if [ "$GOTOOLCHAIN" != local ] || [ "$GOENV" != off ] || [ "$GOPROXY" != off ]; then echo ENV_POLICY_LOST; fi
echo controlled-build-failure
exit 17`)
	path := filepath.Join(state, "plugins/demo.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	output, err := pluginUpdateLocal(t, state, installPlainLines, "y\n", "demo", folder)
	after, readErr := os.ReadFile(path)
	if err == nil || readErr != nil || !bytes.Equal(before, after) || old.Launch.Verify() != nil || !strings.Contains(err.Error(), "The old version is still installed") {
		t.Fatal("failed build changed previous runnable installation", output, err)
	}
	if !strings.Contains(output, "controlled-build-failure") || strings.Contains(output, "ENV_LEAK") || strings.Contains(output, "ENV_POLICY_LOST") {
		t.Fatal("update build inherited owner environment or lost build policy", output)
	}
	entries, err := os.ReadDir(filepath.Join(state, "plugins/.staging/demo"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "build.log" {
		t.Fatal("failed update kept copied source", entries, err)
	}
}

func TestPluginUpdateManifestGrammarRefusesBeforeBuild(t *testing.T) {
	state, _, old := installedUpdateFolder(t, "demo")
	marker := filepath.Join(t.TempDir(), "unexpected-build")
	folder := fakeGoFolder(t, ": > '"+marker+"'")
	path := filepath.Join(folder, "zalki-plugin.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteInstallManifest(t, folder, []byte(strings.ReplaceAll(string(raw), `"build","-o"`, `"build","-gcflags=all=-N","-o"`)))
	output, err := pluginUpdateLocal(t, state, installPlainLines, "y\n", "demo", folder)
	if err == nil || strings.Contains(output, updateQuestion) {
		t.Fatal("invalid update build grammar reached review", output, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("invalid update argv executed build", err)
	}
	if now := readInstallStatus(t, state, "demo"); now.Install.TreeSHA256 != old.Install.TreeSHA256 || old.Launch.Verify() != nil {
		t.Fatal("invalid manifest changed installation")
	}
}

func TestPluginInstallDryRunBuildAndCleanup(t *testing.T) {
	binary := checkRealBinary(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("ZAL15_SECRET_CREDENTIAL", "private-sentinel")
	folder := fakeGoFolder(t, `if [ "${ZAL15_SECRET_CREDENTIAL+x}" = x ]; then echo ENV_LEAK; exit 1; fi
printf '#!/bin/sh\nexit 0\n' > plugin
chmod 700 plugin`)
	state := filepath.Join(t.TempDir(), "must-stay-absent")
	output, code := runUpdateBinary(t, binary, state, "plugins", "install", "--dry-run", folder)
	if code != 0 || !strings.Contains(output, "Dry run passed:") {
		t.Fatal("dry-run build did not use isolated shared build helper", output, code)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatal("dry-run retained private build/cache/log/snapshot", entries, err)
	}
	tool := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	if err := os.WriteFile(filepath.Join(tool, "go"), []byte("#!/bin/sh\necho controlled-dry-failure\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	output, code = runUpdateBinary(t, binary, state, "plugins", "install", "--dry-run", folder)
	if code != 1 || !strings.Contains(output, "controlled-dry-failure") || !strings.Contains(output, "log was not kept") {
		t.Fatal("dry-run build failure was hidden", output, code)
	}
	entries, err = os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed dry-run retained private build/cache/log/snapshot", entries, err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry-run build created state", err)
	}
}

func TestPluginUpdateWrongAuthorRefusesBeforeReviewAndBuild(t *testing.T) {
	state, _, _ := installedUpdateFolder(t, "demo")
	marker := filepath.Join(t.TempDir(), "must-not-build")
	folder := fakeGoFolder(t, ": > '"+marker+"'; printf '#!/bin/sh\\nexit 0\\n' > plugin; chmod 700 plugin")
	path := filepath.Join(folder, "zalki-plugin.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteInstallManifest(t, folder, []byte(strings.ReplaceAll(string(raw), `"id":"local/demo"`, `"id":"local/other"`)))
	output, err := pluginUpdateLocal(t, state, installPlainLines, "y\n", "demo", folder)
	if err == nil || strings.Contains(output, updateQuestion) {
		t.Fatal("wrong-author source reached review or build", output, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("wrong-author source reached review or build", output, err)
	}
}

func TestPluginUpdateDisabledStaysDisabled(t *testing.T) {
	state, folder, _ := installedUpdateFolder(t, "demo")
	if output, err := pluginOwnerLocal(t, state, "disable", "demo"); err != nil {
		t.Fatal(string(output), err)
	}
	old := readInstallStatus(t, state, "demo")
	changeUpdateFolder(t, folder)
	output, err := pluginUpdateLocal(t, state, installPlainLines, "y\n", "demo")
	if err != nil {
		t.Fatal(output, err)
	}
	now := readInstallStatus(t, state, "demo")
	if now.Enabled || now.Epoch != old.Epoch || strings.Contains(output, "Enabling...") || !strings.Contains(output, "Publishing update...") || !strings.Contains(output, "zalki plugins enable demo && zalki plugins run demo") {
		t.Fatal("disabled update enabled or misreported the plugin", output)
	}
}

func TestPluginUpdateDisabledEmptyExplainsGrantBeforeEnable(t *testing.T) {
	state, folder, _ := installedUpdateFolder(t, "demo")
	if output, err := pluginOwnerLocal(t, state, "revoke", "demo"); err != nil {
		t.Fatal(string(output), err)
	}
	changeUpdateFolder(t, folder)
	output, err := pluginUpdateLocal(t, state, installPlainLines, "y\n", "demo")
	if err != nil {
		t.Fatal(output, err)
	}
	now := readInstallStatus(t, state, "demo")
	if now.Enabled || now.Access.Status || now.Access.Receive || !strings.Contains(output, "zalki plugins grant demo --status && zalki plugins enable demo && zalki plugins run demo") {
		t.Fatal("empty disabled update granted access or gave unusable start guidance", output)
	}
}
