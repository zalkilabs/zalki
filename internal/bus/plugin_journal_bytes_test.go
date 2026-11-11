package bus

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	byteFixtureActor   = "cursor:00000000-0000-4000-8000-000000000071"
	byteFixtureGen     = "00000000-0000-4000-8000-000000000072"
	byteFixturePlugin  = "00000000-0000-4000-8000-000000000073"
	byteFixtureGrant   = "00000000-0000-4000-8000-000000000074"
	byteFixtureInbox   = "00000000-0000-4000-8000-000000000075"
	byteFixtureNative  = "00000000-0000-4000-8000-000000000076"
	byteFixtureRequest = "00000000-0000-4000-8000-000000000077"
)

func TestPluginJournalPersistedBytes(t *testing.T) {
	f := NewPluginFeed(fixtureStore(t), "golden")
	now := time.Date(2026, 11, 10, 20, 0, 0, 123000000, time.UTC)
	if err := f.write(func(view JournalView) error { return seedPluginJournalBytes(f, view, now) }); err != nil {
		t.Fatal(err)
	}
	before, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	request := auditPluginJournalBytes(t, f, &before, now.Add(time.Second))
	paths := map[string]string{
		"installation":   f.pluginPath(),
		"identity hint":  filepath.Join(f.pluginDir(), "id-"+byteFixturePlugin+".json"),
		"inbox index":    filepath.Join(f.pluginDataPath(), "inbox.json"),
		"inbox message":  filepath.Join(f.store.Dir, "messages", byteFixtureInbox+".json"),
		"native message": filepath.Join(f.store.Dir, "messages", byteFixtureNative+".json"),
		"native audit":   filepath.Join(f.store.Dir, "plugin-read-audit.json"),
	}
	for name, path := range paths {
		assertPluginJournalBytes(t, name, readCommitFile(t, path))
	}
	raw := readCommitFile(t, filepath.Join(f.store.Dir, "plugin-inbox-read-audit.json"))
	// The owner inbox read allocates its own UUID. Compare every other byte.
	if bytes.Count(raw, []byte(request)) != 1 {
		t.Fatal("audit did not record exactly one generated request ID")
	}
	assertPluginJournalBytes(t, "inbox audit", bytes.ReplaceAll(raw, []byte(request), []byte(byteFixtureRequest)))
}

func seedPluginJournalBytes(f PluginFeed, view JournalView, now time.Time) error {
	for _, dir := range []string{f.pluginDir(), f.pluginDataPath()} {
		if err := pluginDirectory(dir, true); err != nil {
			return err
		}
	}
	launch := &PluginLaunch{Executable: "/fixture/plugin", Args: []string{"literal <>&"}, SHA256: strings.Repeat("a", 64), PinSHA256: strings.Repeat("b", 64)}
	capability := &PluginCapabilities{Version: 1, Installation: byteFixturePlugin, PinSHA256: launch.PinSHA256, Revision: byteFixtureGrant,
		PluginCapabilityGrants: PluginCapabilityGrants{Read: []string{byteFixtureActor}, InboxRead: true, InboxFrom: []string{byteFixtureActor}}}
	if err := f.savePluginIdentity(byteFixturePlugin); err != nil {
		return err
	}
	if _, err := f.savePlugin(pluginRecord{ID: byteFixturePlugin, Name: f.name, Launch: launch, Enabled: true, Sessions: []string{byteFixtureActor}, GapReason: "enabled", Capabilities: capability}); err != nil {
		return err
	}
	return seedPluginJournalMessages(f, view, now)
}

func seedPluginJournalMessages(f PluginFeed, view JournalView, now time.Time) error {
	at := now.Format(time.RFC3339Nano)
	session := model.Session{Schema: recordSchema, NativeID: strings.TrimPrefix(byteFixtureActor, "cursor:"), ID: byteFixtureActor,
		Kind: "cursor", Name: "Golden native", CWD: "/fixture/workspace", Generation: byteFixtureGen, State: model.StateReady, UpdatedAt: at}
	path, err := f.store.sessionPath(session.ID)
	if err != nil {
		return err
	}
	if err := writeJSON(path, session); err != nil {
		return err
	}
	native := model.Message{ID: byteFixtureNative, From: byteFixtureActor, To: byteFixtureActor, TargetGeneration: byteFixtureGen,
		Body: "native <>&", CreatedAt: at, Status: model.StatusSubmitted}
	if err := f.store.saveMessage(native); err != nil {
		return err
	}
	inbox := model.Message{Schema: recordSchema, ID: byteFixtureInbox, From: byteFixtureActor, FromName: session.Name, FromLabel: "Cursor",
		To: "plugin:" + byteFixturePlugin, TargetGeneration: byteFixturePlugin, Body: "inbox <>&", CreatedAt: at,
		Status: model.StatusSubmitted, Receipt: &model.Receipt{At: at, Status: model.StatusSubmitted}}
	index, err := f.loadPluginInboxIndex(view, byteFixturePlugin, now)
	if err != nil {
		return err
	}
	if err := f.reservePluginInbox(index, inbox, byteFixtureGen, byteFixtureGrant, now); err != nil {
		return err
	}
	return f.store.saveMessage(inbox)
}

func auditPluginJournalBytes(t *testing.T, f PluginFeed, before *PluginFeedStatus, now time.Time) string {
	t.Helper()
	var result pluginReadResult
	err := f.write(func(view JournalView) error {
		if _, _, err := f.readPluginMessageAt(view, before, byteFixtureActor, byteFixtureNative, byteFixtureRequest, now); err != nil {
			return err
		}
		raw, err := f.readPluginInboxAt(view, before, byteFixtureInbox, now)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &result)
	})
	if err != nil || !canonicalEventID(result.Request) {
		t.Fatal("could not audit both persisted messages", err)
	}
	return result.Request
}

func TestPluginJournalEventAndRedoBytes(t *testing.T) {
	f := newCommitFixture(t)
	if err := os.Chmod(f.writer.dir, 0700); err != nil {
		t.Fatal(err)
	}
	first := f.event
	first.ID, first.TransactionID = byteFixtureNative, byteFixtureInbox
	first.Type, first.Session.Previous, first.Session.State = "session.registered", "", model.StateReady
	entry, err := journal.EncodeJSON(eventRecord{Epoch: byteFixturePlugin, Sequence: 1, Event: first}, maxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(f.writer.dir, eventRecordPath(1)), entry); err != nil {
		t.Fatal(err)
	}
	meta := eventMetadata{Epoch: byteFixturePlugin, First: 1, Last: 1, Count: 1, Bytes: uint64(len(entry))}
	if err := writeJSON(filepath.Join(f.writer.dir, eventMetaPath), meta); err != nil {
		t.Fatal(err)
	}
	stageCommit(t, f, preparedCommit(t, f))
	assertPluginJournalBytes(t, "redo intent", readCommitFile(t, filepath.Join(f.writer.dir, "commits", "pending.json")))
	store := Store{Dir: f.writer.dir, Registry: testRegistry()}
	if err := WithJournalRead(store, func(view JournalView) error {
		event, err := view.EventAt(byteFixturePlugin, 2)
		if err != nil || event != f.event {
			t.Fatal("view did not recover the fixed event", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertPluginJournalBytes(t, "event record", readCommitFile(t, filepath.Join(f.writer.dir, eventRecordPath(2))))
	assertPluginJournalBytes(t, "event metadata", readCommitFile(t, filepath.Join(f.writer.dir, eventMetaPath)))
}

func assertPluginJournalBytes(t *testing.T, name string, raw []byte) {
	t.Helper()
	// Captured with production files at 149740f before the journal boundary.
	want := map[string]string{
		"installation":   "22cc4b86b83f09f3b48f8f73f1c0da2604b8976bdfabfeb6f421f33d328a1539",
		"identity hint":  "6427652d690a10e557e0354e70a402d50ebe7180f4bbc03bb7bc0848ff2e235b",
		"inbox index":    "cceb38d7cf3a91fdbdb108ef088ddb973d2f04710a7a826c9b6e6df1fcd5eb83",
		"inbox message":  "2fcf250cc799f48e7e63a66ee3cea05e9210c572a57bae3f6971e372f81ca8cb",
		"native message": "6cb482bf5bacc7311c49c6ba2f304f1efd1aa94c9f13252c16beebb8611d5622",
		"native audit":   "0b4d819ba006f36622a358b79086dd851025931a6699a509b8239c63de4a91d0",
		"inbox audit":    "eac9d3bece08f2a9804d03062ceb050f1e2493136f5986b4da7e2f203b960470",
		"redo intent":    "1a7911203480433b504c8831062eb66b37a0887bebd10904975d9562f08a7414",
		"event record":   "6cc6c4d35fcbf4c0e95deb13b643b371fa6ba81b9d88f27cba3a5e5cdd09c1d4",
		"event metadata": "bfd0377c967540dfc41e311bc4a01df29883984fc6788c8c3bae125873b4b60e",
	}[name]
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
		t.Fatalf("%s bytes changed: SHA256 %s, want %s", name, got, want)
	}
}
