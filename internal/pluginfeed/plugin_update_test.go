package pluginfeed

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func updateArtifact(t *testing.T, i *PluginInstall) string {
	t.Helper()
	path := filepath.Join(i.StagingDir(), "new-plugin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func beginTestUpdate(t *testing.T, f PluginFeed) (*PluginInstall, PluginFeedStatus) {
	t.Helper()
	i, status, err := f.BeginPluginUpdate()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := i.Close(); err != nil {
			t.Error(err)
		}
	})
	return i, status
}

func TestPluginUpdatePreservesIdentityDataAndDisabledPosition(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(enabledTextForTest(enabled), func(t *testing.T) {
			checkUpdatePreservesState(t, enabled)
		})
	}
}

func checkUpdatePreservesState(t *testing.T, enabled bool) {
	t.Helper()
	f := installForTest(t, fixtureStore(t))
	if !enabled {
		if err := f.EnablePluginFeed(false); err != nil {
			t.Fatal(err)
		}
	}
	old := feedRecordNamed(t, f)
	data := filepath.Join(f.pluginDataPath(), "settings.json")
	if err := os.WriteFile(data, []byte("owner settings\n"), 0600); err != nil {
		t.Fatal(err)
	}
	i, _ := beginTestUpdate(t, f)
	if _, err := i.Commit(updateArtifact(t, i), []string{"--new"}, old.Access, *old.Install, "", "", true); err != nil {
		t.Fatal(err)
	}
	now := feedRecordNamed(t, f)
	got, err := os.ReadFile(data)
	assertUpdateIdentityAndPin(t, old, now, got, err)
	if enabled && (now.Epoch == old.Epoch || now.GapReason != "enabled") {
		t.Fatal("enabled update did not atomically rebind")
	}
	if !enabled && (now.Epoch != old.Epoch || now.Position != old.Position || now.GapID != old.GapID || now.GapReason != old.GapReason) {
		t.Fatal("disabled update changed cursor or gaps")
	}
	if _, err := os.Stat(old.Launch.Executable); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old pin not swept", err)
	}
}

func enabledTextForTest(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func TestPluginUpdateSameArtifactKeepsBindingAndPin(t *testing.T) {
	f := installForTest(t, fixtureStore(t))
	old := feedRecordNamed(t, f)
	before, err := os.Stat(old.Launch.Executable)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := beginTestUpdate(t, f)
	receipt := *old.Install
	receipt.Source = "/moved/demo"
	if _, err := i.Commit(stagedTestArtifact(t, i), old.Launch.Args, old.Access, receipt, "", "", true); err != nil {
		t.Fatal(err)
	}
	now := feedRecordNamed(t, f)
	after, err := os.Stat(old.Launch.Executable)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("same artifact pin was replaced or deleted", err)
	}
	if now.Revision != old.Revision || now.Epoch != old.Epoch || now.Position != old.Position || now.GapID != old.GapID {
		t.Fatal("receipt-only update changed the binding or gap")
	}
	if now.Install.Source != receipt.Source {
		t.Fatal("moved source was not recorded")
	}
}

func TestPluginUpdateSameArtifactNewArgumentsRebinds(t *testing.T) {
	f := installForTest(t, fixtureStore(t))
	i, status := beginTestUpdate(t, f)
	if _, err := i.Commit(stagedTestArtifact(t, i), []string{"--changed"}, status.Access, *status.Install, "", "", true); err != nil {
		t.Fatal(err)
	}
	now := feedRecordNamed(t, f)
	if now.Launch.SHA256 != status.Launch.SHA256 || now.Launch.PinSHA256 == status.Launch.PinSHA256 || now.Revision == status.Revision || now.Epoch == status.Epoch {
		t.Fatal("same bytes with changed argv did not rebind the semantic pin")
	}
}

func TestPluginUpdateRefusesChangedRecord(t *testing.T) {
	f := installForTest(t, fixtureStore(t))
	i, status := beginTestUpdate(t, f)
	previous := beforePluginInstallCommit
	beforePluginInstallCommit = func() {
		if err := f.ConfigurePluginScope("none"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforePluginInstallCommit = previous })
	_, err := i.Commit(updateArtifact(t, i), nil, status.Access, *status.Install, "", "", true)
	if err == nil || !strings.Contains(err.Error(), "changed while you reviewed") {
		t.Fatal("changed record was overwritten", err)
	}
	now := feedRecordNamed(t, f)
	if now.Scope != "none" || now.Launch.PinSHA256 != status.Launch.PinSHA256 || status.Launch.Verify() != nil {
		t.Fatal("refused update damaged previous runnable record")
	}
}

func TestPluginUpdatePublicationOutcomes(t *testing.T) {
	for _, outcome := range []string{"old", "new", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			checkUpdatePublicationOutcome(t, outcome)
		})
	}
}

func checkUpdatePublicationOutcome(t *testing.T, outcome string) {
	t.Helper()
	f := installForTest(t, fixtureStore(t))
	i, status := beginTestUpdate(t, f)
	prior, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	original := savePluginInstallRecord
	savePluginInstallRecord = func(s PluginFeed, p pluginRecord) ([]byte, error) {
		p.Revision = pluginRevision(p)
		written, err := journal.EncodeJSON(p, 16<<10)
		if err != nil {
			return nil, err
		}
		switch outcome {
		case "new":
			err = journal.WriteAtomic(s.pluginPath(), written)
		case "unknown":
			err = os.WriteFile(s.pluginPath(), []byte("unknown bytes"), 0600)
		}
		return written, errors.Join(err, errors.New("injected publication failure"))
	}
	t.Cleanup(func() { savePluginInstallRecord = original })
	kept, err := i.Commit(updateArtifact(t, i), nil, status.Access, *status.Install, "", "", true)
	want := map[string]string{"old": "update was not written", "new": "update was written", "unknown": "update may or may not be in effect"}[outcome]
	if err == nil || !strings.Contains(err.Error(), want) || kept != (outcome != "old") {
		t.Fatal("publication outcome not classified by exact bytes", kept, err)
	}
	if status.Launch.Verify() != nil {
		t.Fatal("publication failure removed the old runnable pin")
	}
	entries, err := os.ReadDir(f.pluginBinPath())
	if err != nil || len(entries) != 2 {
		t.Fatal("uncertain publication did not retain both pins", entries, err)
	}
	if outcome == "old" {
		raw, err := os.ReadFile(f.pluginPath())
		if err != nil || !bytes.Equal(raw, prior) {
			t.Fatal("old outcome changed the record")
		}
	}
}

func TestPluginUpdateKeepsForeignArtifacts(t *testing.T) {
	f := installForTest(t, fixtureStore(t))
	old := feedRecordNamed(t, f)
	foreign := filepath.Join(f.pluginBinPath(), "owner-notes")
	if err := os.WriteFile(foreign, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if i, _, err := f.BeginPluginUpdate(); err == nil {
		_ = i.Close()
		t.Fatal("update accepted a foreign artifact")
	}
	if raw, err := os.ReadFile(foreign); err != nil || string(raw) != "keep me" || old.Launch.Verify() != nil {
		t.Fatal("update swept a foreign file or current pin")
	}
}

func TestPluginUpdateLeaseRefusalNamesCommand(t *testing.T) {
	f := installForTest(t, fixtureStore(t))
	lease, err := f.AcquirePluginLease()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	i, _, err := f.BeginPluginUpdate()
	if i != nil {
		_ = i.Close()
	}
	if i != nil || err == nil || !strings.Contains(err.Error(), "zalki plugins disable demo") || !strings.Contains(err.Error(), "zalki plugins update demo") {
		t.Fatal("lease refusal lacks exact recovery command", err)
	}
}

func TestPluginUpdateAuthenticatesCurrentInstaller(t *testing.T) {
	s := fixtureStore(t)
	session, key := register(t, s, "codex")
	f := installForTest(t, s)
	i, status := beginTestUpdate(t, f)
	receipt := *status.Install
	receipt.InstalledBy = "terminal"
	if _, err := i.Commit(updateArtifact(t, i), nil, status.Access, receipt, session.ID, key, false); err == nil {
		t.Fatal("extended update accepted without review")
	}
	if _, err := i.Commit(updateArtifact(t, i), nil, status.Access, receipt, session.ID, key, true); err != nil {
		t.Fatal(err)
	}
	now := feedRecordNamed(t, f)
	if now.Install.InstalledBy != session.ID {
		t.Fatal("update did not authenticate current installer")
	}
}

func TestPluginUpdateReadOnlyStatusMissingState(t *testing.T) {
	f := NewPluginFeed(fixtureStore(t), "demo")
	f.store.Dir = filepath.Join(t.TempDir(), "missing")
	f.root = f.store.Dir
	status, err := f.ReadPluginUpdateStatus()
	if err != nil || status.ID != "" {
		t.Fatal(status, err)
	}
	if _, err := os.Stat(f.store.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only update status created state", err)
	}
}

func TestPluginUpdateCommitRechecksPin(t *testing.T) {
	f := installForTest(t, fixtureStore(t))
	i, status := beginTestUpdate(t, f)
	old := beforePluginInstallCommit
	beforePluginInstallCommit = func() {
		entries, err := os.ReadDir(f.pluginBinPath())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			path := filepath.Join(f.pluginBinPath(), e.Name())
			if path != status.Launch.Executable {
				if err := os.Chmod(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	t.Cleanup(func() { beforePluginInstallCommit = old })
	_, err := i.Commit(updateArtifact(t, i), nil, status.Access, *status.Install, "", "", true)
	if err == nil {
		t.Fatal("update accepted a changed pinned artifact")
	}
	now := feedRecordNamed(t, f)
	if !reflect.DeepEqual(now.Launch, status.Launch) || status.Launch.Verify() != nil {
		t.Fatal("pin refusal changed the previous runnable launch")
	}
}

func TestPluginUpdateRejectsUnreviewedExtendedAccess(t *testing.T) {
	f := installForTest(t, fixtureStore(t))
	i, status := beginTestUpdate(t, f)
	_, err := i.Commit(updateArtifact(t, i), nil, status.Access, *status.Install, "", "", false)
	if model.ErrorCodeOf(err) != model.CodeInvalidInput || !strings.Contains(err.Error(), "own terminal") {
		t.Fatal("update bypassed terminal-review proof", err)
	}
}

func assertUpdateIdentityAndPin(t *testing.T, old, now pluginRecord, got []byte, err error) {
	t.Helper()
	if err != nil || string(got) != "owner settings\n" || now.ID != old.ID || now.Enabled != old.Enabled || now.Scope != old.Scope {
		t.Fatal("update lost identity, data, scope or enabled state", err)
	}
	if now.Revision == old.Revision || now.Launch.PinSHA256 == old.Launch.PinSHA256 || now.Launch.Verify() != nil {
		t.Fatal("update did not bind the new pin and revision")
	}
}

func standardUpdateFeed(t *testing.T) PluginFeed {
	t.Helper()
	f := NewPluginFeed(fixtureStore(t), "demo")
	i, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.Commit(stagedTestArtifact(t, i), nil, PluginAccess{Status: true}, standardInstallReceipt(), "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := i.Close(); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPluginUpdateBackendRequiresAccessReview(t *testing.T) {
	for _, kind := range []string{"unchanged", "grant growth", "declaration change", "reviewed growth", "disabled empty"} {
		t.Run(kind, func(t *testing.T) { checkUpdateBackendReview(t, kind) })
	}
}

func checkUpdateBackendReview(t *testing.T, kind string) {
	t.Helper()
	f := standardUpdateFeed(t)
	if kind == "disabled empty" {
		if err := f.RevokePluginAccess(); err != nil {
			t.Fatal(err)
		}
	}
	i, status := beginTestUpdate(t, f)
	receipt := *status.Install
	access := status.Access
	if kind == "grant growth" || kind == "reviewed growth" {
		access.Receive = true
	}
	if kind == "declaration change" {
		receipt.Access = []byte(`{"status":true,"receive":false,"send":false,"read":false}`)
	}
	before, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = i.Commit(updateArtifact(t, i), nil, access, receipt, "", "", kind == "reviewed growth")
	wantRefusal := kind == "grant growth" || kind == "declaration change"
	if wantRefusal {
		assertBackendUpdateRefusal(t, err, f, before, status)
		return
	}
	if err != nil {
		t.Fatal("backend refused unchanged or reviewed update", kind, err)
	}
	if kind == "disabled empty" && feedRecordNamed(t, f).Enabled {
		t.Fatal("empty disabled update was enabled")
	}
}

func assertBackendUpdateRefusal(t *testing.T, err error, f PluginFeed, before []byte, status PluginFeedStatus) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "changed access needs the review screen") {
		t.Fatal("backend accepted an unreviewed standard access change", err)
	}
	after, readErr := os.ReadFile(f.pluginPath())
	if readErr != nil || !bytes.Equal(before, after) || status.Launch.Verify() != nil {
		t.Fatal("backend access refusal damaged old installation")
	}
}

func TestPluginUpdateRejectsDifferentReceiptAuthor(t *testing.T) {
	f := standardUpdateFeed(t)
	i, status := beginTestUpdate(t, f)
	receipt := *status.Install
	receipt.ID = "local/other"
	before, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = i.Commit(updateArtifact(t, i), nil, status.Access, receipt, "", "", true)
	if err == nil || !strings.Contains(err.Error(), "different plugin") {
		t.Fatal("backend accepted a different manifest author ID", err)
	}
	after, err := os.ReadFile(f.pluginPath())
	if err != nil || !bytes.Equal(before, after) || status.Launch.Verify() != nil {
		t.Fatal("wrong-author refusal changed old installation")
	}
}

func TestPluginUpdateSweepRefusesInterleavedForeignEntry(t *testing.T) {
	for _, kind := range []string{"added", "replaced"} {
		t.Run(kind, func(t *testing.T) { checkInterleavedUpdatePin(t, kind) })
	}
}

func checkInterleavedUpdatePin(t *testing.T, kind string) {
	t.Helper()
	f := installForTest(t, fixtureStore(t))
	current := feedRecordNamed(t, f).Launch
	source := filepath.Join(t.TempDir(), "orphan")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	orphan, err := f.copyPluginArtifact(source)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(f.pluginBinPath(), "owner-notes")
	if kind == "replaced" {
		foreign = orphan
	}
	saved := filepath.Join(t.TempDir(), "saved-orphan")
	old := beforeUpdatePinSweep
	beforeUpdatePinSweep = func() {
		if kind == "replaced" {
			if err := os.Rename(orphan, saved); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(foreign, []byte("foreign sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeUpdatePinSweep = old })
	i, _, err := f.BeginPluginUpdate()
	if i != nil {
		_ = i.Close()
	}
	if err == nil {
		t.Fatal("sweep accepted changed inventory or deleted before proof")
	}
	raw, readErr := os.ReadFile(foreign)
	if readErr != nil || string(raw) != "foreign sentinel" || current.Verify() != nil {
		t.Fatal("sweep removed interleaved foreign sentinel or current pin", err, readErr)
	}
	if kind == "added" && !pinMatches(orphan) {
		t.Fatal("sweep deleted an orphan before completing ownership proof")
	}
}

func TestPluginUpdateBeginningSweepsOnlyOrphans(t *testing.T) {
	f := installForTest(t, fixtureStore(t))
	current := feedRecordNamed(t, f).Launch
	info, err := os.Stat(current.Executable)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "orphan")
	if err := os.WriteFile(source, []byte("#!/bin/sh\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	orphan, err := f.copyPluginArtifact(source)
	if err != nil {
		t.Fatal(err)
	}
	_, status := beginTestUpdate(t, f)
	after, err := os.Stat(current.Executable)
	if err != nil || status.Launch.Verify() != nil || !os.SameFile(info, after) {
		t.Fatal("begin update swept the current runnable pin", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("begin update kept a proven orphan pin", err)
	}
}
