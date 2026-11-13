package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/plugins"
)

type observedInstallInput struct{ reads int }

func (in *observedInstallInput) Read([]byte) (int, error) { in.reads++; return 0, io.EOF }

func executeInstallTest(t *testing.T, state string, in io.Reader, out io.Writer, args ...string) error {
	t.Helper()
	cmd := newRootCommand(t.TempDir())
	cmd.SetIn(in)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(append([]string{"--state", state, "--log-level", "off", "plugins", "install"}, args...))
	return cmd.ExecuteContext(t.Context())
}

func TestPluginInstallYesNoStdin(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	folder := nativeInstallFolder(t)
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := &observedInstallInput{}
	var output bytes.Buffer
	err = executeInstallTest(t, store.Dir, in, &output, folder, "--yes", "--allow-unsandboxed", "--tree-sha256", snapshot.TreeSHA256)
	if in.reads != 0 {
		t.Fatal("standard --yes read stdin", in.reads)
	}
	if err != nil || strings.Contains(output.String(), installQuestion) {
		t.Fatal("standard --yes required a question", output.String(), err)
	}
}

func TestPluginInstallYesGuards(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"missing hash", nil, "With --yes, provide --tree-sha256"},
		{"wrong hash", []string{"--tree-sha256", strings.Repeat("0", 64)}, "The source changed:"},
		{"missing unsandboxed", []string{"--tree-sha256", "MATCH"}, "With --yes, add --allow-unsandboxed"},
	} {
		t.Run(c.name, func(t *testing.T) { checkPluginYesGuard(t, c.name, c.args, c.want) })
	}
}

func TestPluginInstallOldFlagsRemoved(t *testing.T) {
	for _, flag := range []string{"session", "all-sessions", "read", "send", "receive", "accept-from", "inbox-read", "set"} {
		t.Run(flag, func(t *testing.T) {
			store, _ := pluginOwnerFixture(t)
			var output bytes.Buffer
			err := executeInstallTest(t, store.Dir, strings.NewReader("y\n"), &output, "DOES_NOT_EXIST", "--"+flag, "value")
			if err == nil || !strings.Contains(err.Error(), "unknown flag: --"+flag) || strings.Contains(output.String(), "Preparing...") {
				t.Fatal("removed flag was accepted or reached preparation", output.String(), err)
			}
			assertNothingInstalled(t, store.Dir)
		})
	}
}

type failedQuestionWriter struct{ bytes.Buffer }

func (w *failedQuestionWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(installQuestion)) {
		return 0, errors.New("PRIVATE_WRITER_ERROR")
	}
	return w.Buffer.Write(p)
}
func (w *failedQuestionWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestPluginInstallReviewWriteError(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	mark := filepath.Join(t.TempDir(), "BUILD")
	folder := fakeGoFolder(t, ": > '"+mark+"';printf built > plugin;chmod 700 plugin")
	previous := installTerminal
	installTerminal = func(io.Reader) (installChoice, func()) { return installPlainLines, func() {} }
	defer func() { installTerminal = previous }()
	output := &failedQuestionWriter{}
	err := executeInstallTest(t, store.Dir, strings.NewReader("yes\n"), output, folder)
	if _, statErr := os.Stat(mark); !os.IsNotExist(statErr) {
		t.Fatal("failed review question executed BUILD", statErr)
	}
	if err == nil || strings.Contains(err.Error(), "PRIVATE_WRITER_ERROR") {
		t.Fatal("failed question was ignored or leaked writer text", err)
	}
	assertNothingInstalled(t, store.Dir)
}

func TestPluginInstallDetailsDoNotApprove(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	mark := filepath.Join(t.TempDir(), "BUILD")
	folder := fakeGoFolder(t, ": > '"+mark+"';printf built > plugin;chmod 700 plugin")
	output, err := pluginInstallLocal(t, store.Dir, installPlainLines, "n\n", folder, "--details")
	if _, statErr := os.Stat(mark); !os.IsNotExist(statErr) {
		t.Fatal("details or decline executed BUILD", statErr)
	}
	if err != nil || strings.Count(output, installQuestion) != 1 || !strings.Contains(output, "Purpose (author claim): cancel test") || !strings.Contains(output, "Read the copied source:") || strings.Index(output, "Purpose (author claim):") > strings.Index(output, installQuestion) {
		t.Fatal("details did not precede sole explicit consent", output, err)
	}
	assertNothingInstalled(t, store.Dir)
}

func TestPluginInstallNoImplicitStart(t *testing.T) {
	for _, yes := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "yes"}[yes], func(t *testing.T) {
			store, _ := pluginOwnerFixture(t)
			mark := filepath.Join(t.TempDir(), "RUN")
			folder := writeInstallFolder(t, nativeInstallManifest, map[string]string{"bin/plugin": "#!/bin/sh\n: > '" + mark + "'\n"})
			args := []string{folder}
			mode := installRawKeys
			if yes {
				snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				args = append(args, "--yes", "--allow-unsandboxed", "--tree-sha256", snapshot.TreeSHA256)
				mode = installNoTerminal
			}
			output, err := pluginInstallLocal(t, store.Dir, mode, "y", args...)
			if err != nil {
				t.Fatal(output, err)
			}
			if _, statErr := os.Stat(mark); !os.IsNotExist(statErr) {
				t.Fatal("install implicitly started plugin", statErr)
			}
			if !strings.Contains(output, "Installed. Start it with: zalki plugins run demo") {
				t.Fatal("manual start route missing", output)
			}
		})
	}
}

func TestPluginInstallerProofCapturedAndRechecked(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured", true: "rotated"}[rotate], func(t *testing.T) { checkPluginInstallerProof(t, rotate) })
	}
}

func checkPluginYesGuard(t *testing.T, name string, guardArgs []string, want string) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	mark := filepath.Join(t.TempDir(), "BUILD")
	folder := fakeGoFolder(t, ": > '"+mark+"';printf built > plugin;chmod 700 plugin")
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	args := []string{folder, "--yes"}
	if name != "missing unsandboxed" {
		args = append(args, "--allow-unsandboxed")
	}
	for _, arg := range guardArgs {
		if arg == "MATCH" {
			arg = snapshot.TreeSHA256
		}
		args = append(args, arg)
	}
	var output bytes.Buffer
	err = executeInstallTest(t, store.Dir, strings.NewReader("y\n"), &output, args...)
	if _, statErr := os.Stat(mark); !os.IsNotExist(statErr) {
		t.Fatal("--yes guard executed BUILD", statErr)
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatal("--yes guard did not refuse for intended reason", output.String(), err)
	}
	assertNothingInstalled(t, store.Dir)
}

func checkPluginInstallerProof(t *testing.T, rotate bool) {
	t.Helper()
	store, session := pluginOwnerFixture(t)
	own, key, err := store.Register("codex", session.NativeID, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_ACTOR", own.ID)
	t.Setenv("ZALKI_CAPABILITY", key)
	previous := installStep
	installStep = func(step string) {
		if step == "commit" {
			changeInstallerAtCommit(t, store, session.NativeID, rotate)
		}
	}
	defer func() { installStep = previous }()
	installReviewInstance(t, store.Dir, "demo", "0.1.0")
	status := readInstallStatus(t, store.Dir, "demo")
	want := own.ID
	if rotate {
		want = "unproven"
	}
	if status.Install == nil || status.Install.InstalledBy != want {
		t.Fatal("installer proof was not captured and rechecked", status.Install)
	}
	raw, err := os.ReadFile(filepath.Join(store.Dir, "plugins", "demo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(key)) {
		t.Fatal("installer capability stored in receipt")
	}
	if rotate && status.Install.InstalledByLabel != "" {
		t.Fatal("unproven installer acquired a session label")
	}
}

func changeInstallerAtCommit(t *testing.T, store bus.Store, native string, rotate bool) {
	t.Helper()
	if rotate {
		if _, _, err := store.Register("codex", native, t.TempDir(), nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv("ZALKI_ACTOR", "BAD")
	t.Setenv("ZALKI_CAPABILITY", "BAD")
}
