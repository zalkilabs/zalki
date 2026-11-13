package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/plugins"
)

// The expected digest matches the final copy, so a hash mismatch or Commit
// refusal cannot conceal executing a build before the authoritative guard.
func TestPluginCopiedManifestRefusesBeforeBuild(t *testing.T) {
	for _, kind := range []string{"send", "read"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := pluginOwnerFixture(t)
			mark := filepath.Join(t.TempDir(), "BUILD")
			folder := fakeGoFolder(t, ": > '"+mark+"'; printf built > plugin; chmod 700 plugin")
			initial, final, snapshot := changedInstallSnapshot(t, folder, kind)
			previous := installStep
			installStep = func(step string) {
				if step == "snapshot" {
					mustWriteInstallManifest(t, folder, final)
				}
			}
			defer func() { installStep = previous }()
			mustWriteInstallManifest(t, folder, initial)
			output, err := pluginInstallLocal(t, store.Dir, installNoTerminal, "", folder, "--yes", "--allow-unsandboxed", "--tree-sha256", snapshot.TreeSHA256)
			if _, statErr := os.Stat(mark); !os.IsNotExist(statErr) {
				t.Fatal("copied extended access executed BUILD before refusal", statErr)
			}
			if err == nil || !strings.Contains(err.Error(), "asks for extended access ("+strings.ToUpper(kind)+")") || strings.Contains(output, "Building...") {
				t.Fatal("copied extended declaration not refused by early guard", output, err)
			}
			assertNothingInstalled(t, store.Dir)
		})
	}
}

func changedInstallSnapshot(t *testing.T, folder, kind string) ([]byte, []byte, plugins.InstallSource) {
	t.Helper()
	initial, err := os.ReadFile(filepath.Join(folder, "zalki-plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	final := bytes.Replace(initial, []byte(`"`+kind+`":false`), []byte(`"`+kind+`":true`), 1)
	if bytes.Equal(initial, final) {
		t.Fatal("access fixture did not change")
	}
	mustWriteInstallManifest(t, folder, final)
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return initial, final, snapshot
}

func mustWriteInstallManifest(t *testing.T, folder string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(folder, "zalki-plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func copiedAccessInstall(t *testing.T) (string, installStatus, plugins.InstallSource) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	initial, final, snapshot := changedInstallSnapshot(t, folder, "send")
	previous := installStep
	installStep = func(step string) {
		if step == "snapshot" {
			mustWriteInstallManifest(t, folder, final)
		}
	}
	defer func() { installStep = previous }()
	mustWriteInstallManifest(t, folder, initial)
	output, err := pluginInstallLocal(t, store.Dir, installPlainLines, "\ny\n", folder)
	if err != nil {
		t.Fatal(output, err)
	}
	return output, readInstallStatus(t, store.Dir, "demo"), snapshot
}

func TestPluginReviewBindsCopiedAccess(t *testing.T) {
	output, _, _ := copiedAccessInstall(t)
	if !strings.Contains(output, "demo requests SEND access.") || !strings.Contains(output, "  SEND messages on its own to: nobody") {
		t.Fatal("review hid copied SEND declaration", output)
	}
	if strings.Count(output, installQuestion) != 1 {
		t.Fatal("review did not ask exactly once", output)
	}
}

func TestPluginReceiptBindsCopiedAccess(t *testing.T) {
	_, status, snapshot := copiedAccessInstall(t)
	var declared plugins.InstallAccess
	if status.Install == nil || json.Unmarshal(status.Install.Access, &declared) != nil || !declared.Send || declared.Read {
		t.Fatal("receipt did not retain copied declared SEND access", status.Install)
	}
	if status.Install.TreeSHA256 != snapshot.TreeSHA256 || status.Install.ManifestSHA256 != snapshot.ManifestSHA256 {
		t.Fatal("receipt hashes do not bind the copied snapshot", status.Install)
	}
	if len(status.Access.Send) != 0 || !status.Access.Status || !status.Access.Receive {
		t.Fatal("empty optional selection fabricated effective grants", status.Access)
	}
}
