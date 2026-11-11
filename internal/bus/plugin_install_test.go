package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const testSHA = "0000000000000000000000000000000000000000000000000000000000000001"

func testInstallReceipt() PluginInstallReceipt {
	return PluginInstallReceipt{Source: "/src/demo", TreeSHA256: testSHA, ManifestSHA256: testSHA, Build: []string{"go", "build"}, Tool: "/usr/bin/go", ToolSHA256: testSHA, Artifact: "plugin", InstalledAt: model.Timestamp()}
}

func stagedTestArtifact(t *testing.T, install *PluginInstall) string {
	t.Helper()
	path := filepath.Join(install.StagingDir(), "plugin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func installForTest(t *testing.T, s Store, session string) PluginFeed {
	t.Helper()
	f := NewPluginFeed(s, "demo")
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	defer install.Close()
	if _, err := install.Commit(stagedTestArtifact(t, install), nil, PluginInstallGrants{Sessions: []string{session}}, testInstallReceipt()); err != nil {
		t.Fatal(err)
	}
	return f
}

func assertNoInstall(t *testing.T, f PluginFeed) {
	t.Helper()
	status, err := f.PluginStatus()
	if err != nil || status.ID != "" {
		t.Fatal("installation was published", status, err)
	}
	for _, path := range []string{f.pluginBinPath(), f.pluginDataPath(), f.stagingPath()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed install left", path, err)
		}
	}
	ids, _ := filepath.Glob(filepath.Join(f.pluginDir(), "id-*.json"))
	if len(ids) != 0 {
		t.Fatal("failed install left identity files", ids)
	}
}

func TestPluginInstallCommitIsOneEnabledRecord(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := NewPluginFeed(s, "demo")
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.BeginPluginInstall(); err == nil {
		t.Fatal("a second attempt got the name lease")
	}
	grants := PluginInstallGrants{Sessions: []string{v.ID}, PluginCapabilityGrants: PluginCapabilityGrants{Read: []string{v.ID}}}
	if _, err := install.Commit(stagedTestArtifact(t, install), []string{"--x"}, grants, testInstallReceipt()); err != nil {
		t.Fatal(err)
	}
	if err := install.Close(); err != nil {
		t.Fatal(err)
	}
	assertCommittedInstall(t, s, f)
}

func assertCommittedInstall(t *testing.T, s Store, f PluginFeed) {
	t.Helper()
	p := feedRecordNamed(t, f)
	if !p.Enabled || p.GapReason != "enabled" || p.Launch == nil || p.Install == nil || p.Capabilities == nil || p.Capabilities.PinSHA256 != p.Launch.PinSHA256 || p.Capabilities.Installation != p.ID || p.Install.ArtifactSHA256 != p.Launch.SHA256 {
		t.Fatalf("record not committed as one enabled installation: %+v", p)
	}
	if filepath.Dir(p.Launch.Executable) != f.pluginBinPath() || p.Launch.Verify() != nil {
		t.Fatal("launch not pinned to the private artifact copy", p.Launch)
	}
	if _, err := os.Lstat(f.stagingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging kept after success", err)
	}
	if target, err := pluginInboxTargetName(s, "plugin:"+p.ID); err != nil || target != "demo" {
		t.Fatal("identity hint missing", target, err)
	}
}

func pluginInboxTargetName(s Store, to string) (string, error) {
	var name string
	err := s.readLocked(func() error {
		f, _, err := pluginInboxTarget(s, to)
		name = f.name
		return err
	})
	return name, err
}

func feedRecordNamed(t *testing.T, f PluginFeed) pluginRecord {
	t.Helper()
	var p pluginRecord
	if err := f.store.readLocked(func() error {
		var err error
		p, _, err = f.loadPlugin()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPluginInstallSwappedArtifactPublishesNothing(t *testing.T) {
	for _, hook := range []*func(){&beforePluginInstallPin, &beforePluginInstallCommit} {
		swapInstalledArtifact(t, hook)
	}
}

func swapInstalledArtifact(t *testing.T, hook *func()) {
	t.Helper()
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := NewPluginFeed(s, "demo")
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	*hook = func() {
		entries, _ := os.ReadDir(f.pluginBinPath())
		for _, entry := range entries {
			path := filepath.Join(f.pluginBinPath(), entry.Name())
			// Rewritten in place: the pin hash, not the inode, must catch it.
			if err := errors.Join(os.Chmod(path, 0700), os.WriteFile(path, []byte("#!/bin/sh\necho swapped\n"), 0700)); err != nil {
				t.Fatal(err)
			}
		}
	}
	defer func() { *hook = func() {} }()
	_, err = install.Commit(stagedTestArtifact(t, install), nil, PluginInstallGrants{Sessions: []string{v.ID}}, testInstallReceipt())
	if model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("swapped artifact was not refused", err)
	}
	if err := install.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoInstall(t, f)
}

func TestPluginInstallCommitFailuresPublishNothing(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := NewPluginFeed(s, "demo")
	bad := testInstallReceipt()
	bad.Source = "relative"
	cases := []struct {
		name    string
		body    string
		mode    os.FileMode
		grants  PluginInstallGrants
		receipt PluginInstallReceipt
		args    []string
	}{
		{"no sessions", "#!/bin/sh\n", 0700, PluginInstallGrants{}, testInstallReceipt(), nil},
		{"bad session", "#!/bin/sh\n", 0700, PluginInstallGrants{Sessions: []string{"nope"}}, testInstallReceipt(), nil},
		{"not executable", "data", 0600, PluginInstallGrants{Sessions: []string{v.ID}}, testInstallReceipt(), nil},
		{"bad receipt", "#!/bin/sh\n", 0700, PluginInstallGrants{Sessions: []string{v.ID}}, bad, nil},
		{"too many args", "#!/bin/sh\n", 0700, PluginInstallGrants{Sessions: []string{v.ID}}, testInstallReceipt(), strings.Fields("1 2 3 4 5 6 7 8 9")},
	}
	for _, c := range cases {
		install, err := f.BeginPluginInstall()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(install.StagingDir(), "plugin")
		if err := os.WriteFile(path, []byte(c.body), c.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := install.Commit(path, c.args, c.grants, c.receipt); err == nil {
			t.Fatal(c.name, "committed")
		}
		if err := install.Close(); err != nil {
			t.Fatal(err)
		}
		assertNoInstall(t, f)
	}
	// A link in place of the staged artifact is refused, not followed.
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(install.StagingDir(), "plugin")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Commit(link, nil, PluginInstallGrants{Sessions: []string{v.ID}}, testInstallReceipt()); err == nil {
		t.Fatal("linked artifact committed")
	}
	if err := install.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoInstall(t, f)
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("link target was touched", err)
	}
}

func TestPluginInstallRefusesExistingAndReservedNames(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := installForTest(t, s, v.ID)
	if _, err := f.BeginPluginInstall(); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("existing installation not refused", err)
	}
	bin := feedRecordNamed(t, f).Launch.Executable
	if _, err := os.Stat(bin); err != nil {
		t.Fatal("refused attempt swept a live artifact", err)
	}
	for _, name := range []string{"fake-notifier", "../x", ".staging", "Demo"} {
		if _, err := NewPluginFeed(s, name).BeginPluginInstall(); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	added := NewPluginFeed(s, "added")
	if err := added.AddPlugin(); err != nil {
		t.Fatal(err)
	}
	if _, err := added.BeginPluginInstall(); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("manually added plugin not refused", err)
	}
}

func TestPluginInstallSweepStaysInsideItsName(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := installForTest(t, s, v.ID)
	other := NewPluginFeed(s, "other")
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	// Leftovers of a crashed attempt are links: only the links are removed.
	if err := os.Symlink(outside, other.pluginBinPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, other.stagingPath()); err != nil {
		t.Fatal(err)
	}
	neighbour := filepath.Join(other.stagingRoot(), "other-2")
	if err := os.Mkdir(neighbour, 0700); err != nil {
		t.Fatal(err)
	}
	install, err := other.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(other.stagingPath()); err != nil || !info.IsDir() {
		t.Fatal("staging was not recreated as a real folder", info, err)
	}
	if err := install.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(other.pluginBinPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bin link kept", err)
	}
	for _, path := range []string{keep, neighbour, feedRecordNamed(t, f).Launch.Executable} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("sweep removed something outside its name", path, err)
		}
	}
}

func TestPluginInstallNeverFollowsLinkedStagingRoot(t *testing.T) {
	s := fixtureStore(t)
	other := NewPluginFeed(s, "other")
	outside := t.TempDir()
	if err := os.MkdirAll(other.pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, other.stagingRoot()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "other"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := other.BeginPluginInstall(); err == nil {
		t.Fatal("linked staging root accepted")
	}
	if err := other.RemovePlugin(); err == nil {
		t.Fatal("remove followed a linked staging root")
	}
	if _, err := os.Stat(filepath.Join(outside, "other")); err != nil {
		t.Fatal("linked staging target was removed", err)
	}
}

func TestPluginInstallRemoveClearsArtifactAndLog(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := NewPluginFeed(s, "demo")
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(install.StagingDir(), "build.log"), []byte("fail"), 0600); err != nil {
		t.Fatal(err)
	}
	install.KeepBuildLog()
	if err := install.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.stagingPath(), "build.log")); err != nil {
		t.Fatal("kept log missing", err)
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.stagingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("remove kept the log", err)
	}
	installForTest(t, s, v.ID)
	if err := f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.pluginBinPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("remove kept the artifact", err)
	}
}

func TestPluginInstallReceiptSurvivesLifecycle(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := installForTest(t, s, v.ID)
	want := feedRecordNamed(t, f).Install
	steps := []func() error{
		func() error { return f.EnablePluginFeed(false) },
		func() error { return f.ConfigurePluginFeed([]string{v.ID}) },
		func() error {
			return f.GrantPluginCapabilities(PluginCapabilityGrants{Read: []string{v.ID}})
		},
		func() error { return f.EnablePluginFeed(true) },
		f.RevokePluginCapabilities,
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatal(i, err)
		}
		if got := feedRecordNamed(t, f).Install; got == nil || got.TreeSHA256 != want.TreeSHA256 || got.InstalledAt != want.InstalledAt {
			t.Fatal("step", i, "dropped the receipt", got)
		}
	}
	status, err := f.PluginStatus()
	if err != nil || status.Install == nil || status.Install.ManifestSHA256 != want.ManifestSHA256 {
		t.Fatal("status receipt missing", status.Install, err)
	}
}

// Persisted poison: every malformed receipt makes the record unreadable
// instead of being repaired or partly trusted.
func TestPluginInstallReceiptPoisonIsRefused(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := installForTest(t, s, v.ID)
	raw, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	original := record["install"]
	for name, value := range receiptPoison(t, original) {
		record["install"] = value
		out, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.WriteAtomic(f.pluginPath(), out); err != nil {
			t.Fatal(err)
		}
		if _, err := f.PluginStatus(); err == nil {
			t.Error(name, "was accepted")
		}
	}
	assertReceiptNeedsLaunch(t, f, record, original)
}

func receiptPoison(t *testing.T, original json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	edit := func(change func(map[string]any)) json.RawMessage {
		var fields map[string]any
		if err := json.Unmarshal(original, &fields); err != nil {
			t.Fatal(err)
		}
		change(fields)
		out, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	return map[string]json.RawMessage{
		"not object":         json.RawMessage(`"x"`),
		"empty object":       json.RawMessage(`{}`),
		"unknown field":      edit(func(m map[string]any) { m["extra"] = 1 }),
		"missing build":      edit(func(m map[string]any) { delete(m, "build") }),
		"null build":         edit(func(m map[string]any) { m["build"] = nil }),
		"null native build":  edit(func(m map[string]any) { m["build"] = nil; delete(m, "tool"); delete(m, "tool_sha256") }),
		"build wrong type":   edit(func(m map[string]any) { m["build"] = "go build" }),
		"nine build args":    edit(func(m map[string]any) { m["build"] = strings.Fields("1 2 3 4 5 6 7 8 9") }),
		"control build arg":  edit(func(m map[string]any) { m["build"] = []string{"go", "a\x1b[2J"} }),
		"missing source":     edit(func(m map[string]any) { delete(m, "source") }),
		"relative source":    edit(func(m map[string]any) { m["source"] = "src" }),
		"dirty source":       edit(func(m map[string]any) { m["source"] = "/a/../b" }),
		"bidi source":        edit(func(m map[string]any) { m["source"] = "/a\u202eb" }),
		"newline source":     edit(func(m map[string]any) { m["source"] = "/a\nb" }),
		"long source":        edit(func(m map[string]any) { m["source"] = "/" + strings.Repeat("a", 1024) }),
		"bad tree":           edit(func(m map[string]any) { m["tree_sha256"] = "zz" }),
		"upper tree":         edit(func(m map[string]any) { m["tree_sha256"] = strings.ToUpper(testSHA[:63]) + "A" }),
		"missing manifest":   edit(func(m map[string]any) { delete(m, "manifest_sha256") }),
		"missing artifact#":  edit(func(m map[string]any) { delete(m, "artifact_sha256") }),
		"escape artifact":    edit(func(m map[string]any) { m["artifact"] = "../x" }),
		"abs artifact":       edit(func(m map[string]any) { m["artifact"] = "/x" }),
		"empty artifact":     edit(func(m map[string]any) { m["artifact"] = "" }),
		"build without tool": edit(func(m map[string]any) { delete(m, "tool") }),
		"relative tool":      edit(func(m map[string]any) { m["tool"] = "go" }),
		"bad tool sha":       edit(func(m map[string]any) { m["tool_sha256"] = "x" }),
		"tool without build": edit(func(m map[string]any) { m["build"] = []string{} }),
		"bad time":           edit(func(m map[string]any) { m["installed_at"] = "yesterday" }),
		"missing time":       edit(func(m map[string]any) { delete(m, "installed_at") }),
	}
}

func assertReceiptNeedsLaunch(t *testing.T, f PluginFeed, record map[string]json.RawMessage, original json.RawMessage) {
	t.Helper()
	// A receipt without a launch is poison too; install null means no receipt.
	record["install"] = original
	delete(record, "launch")
	delete(record, "capabilities")
	out, _ := json.Marshal(record)
	if err := journal.WriteAtomic(f.pluginPath(), out); err != nil {
		t.Fatal(err)
	}
	if _, err := f.PluginStatus(); err == nil {
		t.Error("receipt without launch accepted")
	}
	record["install"] = json.RawMessage(`null`)
	out, _ = json.Marshal(record)
	if err := journal.WriteAtomic(f.pluginPath(), out); err != nil {
		t.Fatal(err)
	}
	if status, err := f.PluginStatus(); err != nil || status.Install != nil {
		t.Fatal("null receipt not treated as absent", status.Install, err)
	}
}

func TestPluginInstallNativeReceiptValid(t *testing.T) {
	r := testInstallReceipt()
	r.Build, r.Tool, r.ToolSHA256, r.ArtifactSHA256 = []string{}, "", "", testSHA
	if !r.valid() {
		t.Fatal("native receipt refused")
	}
	r.Tool = "/usr/bin/go"
	if r.valid() {
		t.Fatal("native receipt with a tool accepted")
	}
}

// The lease excludes other installers; the locked commit still rechecks the
// record and the data folder instead of trusting the earlier checks.
func TestPluginInstallCommitRechecksUnderLock(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := NewPluginFeed(s, "demo")
	changes := []func() error{
		func() error { return f.locked(func() error { return createPluginInstallation(f, nil) }) },
		func() error {
			if err := os.Mkdir(f.pluginDataPath(), 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(f.pluginDataPath(), "old"), nil, 0600)
		},
	}
	for i, change := range changes {
		install, err := f.BeginPluginInstall()
		if err != nil {
			t.Fatal(i, err)
		}
		if err := change(); err != nil {
			t.Fatal(err)
		}
		_, err = install.Commit(stagedTestArtifact(t, install), nil, PluginInstallGrants{Sessions: []string{v.ID}}, testInstallReceipt())
		if model.ErrorCodeOf(err) != model.CodeConflict {
			t.Fatal(i, "commit ignored a change made after Begin", err)
		}
		if err := install.Close(); err != nil {
			t.Fatal(err)
		}
		if p := feedRecordNamed(t, f); p.Enabled || p.Install != nil {
			t.Fatal(i, "commit published over a change", p)
		}
		if err := f.RemovePlugin(); err != nil {
			t.Fatal(err)
		}
	}
}

// Begin refuses abandoned or non-private data before any screen is shown.
func TestPluginInstallBeginRefusesUnsafeData(t *testing.T) {
	s := fixtureStore(t)
	f := NewPluginFeed(s, "demo")
	if err := os.MkdirAll(f.pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	setups := []func() error{
		func() error {
			return errors.Join(os.Mkdir(f.pluginDataPath(), 0700), os.WriteFile(filepath.Join(f.pluginDataPath(), "old"), nil, 0600))
		},
		func() error { return os.Mkdir(f.pluginDataPath(), 0755) },
		func() error { return os.Symlink(empty, f.pluginDataPath()) },
	}
	for i, setup := range setups {
		if err := setup(); err != nil {
			t.Fatal(err)
		}
		if install, err := f.BeginPluginInstall(); err == nil {
			_ = install.Close()
			t.Fatal(i, "unsafe data folder accepted")
		}
		if err := os.RemoveAll(f.pluginDataPath()); err != nil {
			t.Fatal(err)
		}
	}
}

// A save error can come after the rename. The commit must then read the record
// again: a published record keeps everything it points at and reports kept; an
// unpublished one rolls back; an unreadable one keeps everything.
func TestPluginInstallSaveErrorAfterRename(t *testing.T) {
	syncErr := errors.New("injected directory sync failure")
	cases := []struct {
		name      string
		save      func(PluginFeed, pluginRecord) ([]byte, error)
		kept      bool
		published bool
	}{
		{"after rename", func(s PluginFeed, p pluginRecord) ([]byte, error) {
			raw, err := s.savePlugin(p)
			return raw, errors.Join(err, syncErr)
		}, true, true},
		{"before rename", func(PluginFeed, pluginRecord) ([]byte, error) { return nil, syncErr }, false, false},
		// A real failure: with the folder not readable, the rename works and
		// only the directory sync fails.
		{"real directory sync", func(s PluginFeed, p pluginRecord) ([]byte, error) {
			if err := os.Chmod(s.pluginDir(), 0300); err != nil {
				return nil, err
			}
			raw, err := s.savePlugin(p)
			return raw, errors.Join(err, os.Chmod(s.pluginDir(), 0700))
		}, true, true},
		{"unreadable after", func(s PluginFeed, p pluginRecord) ([]byte, error) {
			raw, err := s.savePlugin(p)
			return raw, errors.Join(err, os.WriteFile(s.pluginPath(), []byte("{"), 0600), syncErr)
		}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assertSaveErrorOutcome(t, c.save, c.kept, c.published) })
	}
}

func assertSaveErrorOutcome(t *testing.T, save func(PluginFeed, pluginRecord) ([]byte, error), wantKept, published bool) {
	t.Helper()
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := NewPluginFeed(s, "demo")
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	savePluginInstallRecord = save
	defer func() { savePluginInstallRecord = PluginFeed.savePlugin }()
	kept, err := install.Commit(stagedTestArtifact(t, install), nil, PluginInstallGrants{Sessions: []string{v.ID}}, testInstallReceipt())
	savePluginInstallRecord = PluginFeed.savePlugin
	if err == nil || kept != wantKept {
		t.Fatalf("kept=%v err=%v", kept, err)
	}
	if err := install.Close(); err != nil {
		t.Fatal(err)
	}
	if !kept {
		assertNoInstall(t, f)
		return
	}
	// Kept: the executable, data folder and identity all stay.
	bins, _ := os.ReadDir(f.pluginBinPath())
	ids, _ := filepath.Glob(filepath.Join(f.pluginDir(), "id-*.json"))
	if _, err := os.Stat(f.pluginDataPath()); err != nil || len(bins) != 1 || len(ids) != 1 {
		t.Fatal("kept install lost its files", err, bins, ids)
	}
	if !published {
		return
	}
	status, err := f.PluginStatus()
	if err != nil || !status.Enabled || status.Launch == nil || status.Launch.Verify() != nil || status.Install == nil {
		t.Fatal("published record does not point at a valid install", status, err)
	}
}
