package pluginfeed

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	byteFixtureActor   = "cursor:00000000-0000-4000-8000-000000000071"
	byteFixtureGen     = "00000000-0000-4000-8000-000000000072"
	byteFixturePlugin  = "00000000-0000-4000-8000-000000000073"
	byteFixtureGrant   = "7474747474747474747474747474747474747474747474747474747474747474"
	byteFixtureInbox   = "00000000-0000-4000-8000-000000000075"
	byteFixtureNative  = "00000000-0000-4000-8000-000000000076"
	byteFixtureRequest = "00000000-0000-4000-8000-000000000077"
)

func TestPluginJournalPersistedBytes(t *testing.T) {
	f := NewPluginFeed(fixtureStore(t), "golden")
	now := time.Date(2026, 11, 10, 20, 0, 0, 123000000, time.UTC)
	if err := f.write(func(view bus.JournalView) error { return seedPluginJournalBytes(f, view, now) }); err != nil {
		t.Fatal(err)
	}
	before, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	request := auditPluginJournalBytes(t, f, &before, now.Add(time.Second))
	paths := map[string]string{
		"installation":  f.pluginPath(),
		"identity hint": filepath.Join(f.pluginDir(), "id-"+byteFixturePlugin+".json"),
		"inbox index":   filepath.Join(f.pluginDataPath(), "inbox.json"),
		"native audit":  filepath.Join(f.store.Dir, "plugin-read-audit.json"),
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

func seedPluginJournalBytes(f PluginFeed, view bus.JournalView, now time.Time) error {
	for _, dir := range []string{f.pluginDir(), f.pluginDataPath()} {
		if err := pluginDirectory(dir, true); err != nil {
			return err
		}
	}
	launch := &PluginLaunch{Executable: "/fixture/plugin", Args: []string{"literal <>&"}, SHA256: strings.Repeat("a", 64), PinSHA256: strings.Repeat("b", 64)}
	if err := f.savePluginIdentity(byteFixturePlugin); err != nil {
		return err
	}
	if _, err := f.savePlugin(pluginRecord{ID: byteFixturePlugin, Name: f.name, Launch: launch, Enabled: true, Scope: scopeAll, Access: PluginAccess{Status: true, Receive: true, Read: []string{byteFixtureActor}}, GapReason: "enabled"}); err != nil {
		return err
	}
	return seedPluginJournalMessages(f, view, now)
}

func seedPluginJournalMessages(f PluginFeed, view bus.JournalView, now time.Time) error {
	at := now.Format(time.RFC3339Nano)
	session := model.Session{Schema: recordSchema, NativeID: strings.TrimPrefix(byteFixtureActor, "cursor:"), ID: byteFixtureActor,
		Kind: "cursor", Name: "Golden native", CWD: "/fixture/workspace", Generation: byteFixtureGen, State: model.StateReady, UpdatedAt: at}
	path, err := sessionTestPath(f.store, session.ID)
	if err != nil {
		return err
	}
	if err := writeJSON(path, session); err != nil {
		return err
	}
	native := model.Message{ID: byteFixtureNative, From: byteFixtureActor, To: byteFixtureActor, TargetGeneration: byteFixtureGen,
		Body: "native <>&", CreatedAt: at, Status: model.StatusSubmitted}
	if err := saveTestMessage(f.store, native); err != nil {
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
	return saveTestMessage(f.store, inbox)
}

func auditPluginJournalBytes(t *testing.T, f PluginFeed, before *PluginFeedStatus, now time.Time) string {
	t.Helper()
	var result pluginReadResult
	err := f.write(func(view bus.JournalView) error {
		if _, _, err := f.readPluginMessageAt(view, before, byteFixtureActor, byteFixtureNative, byteFixtureRequest, now); err != nil {
			return err
		}
		raw, err := f.readPluginInboxAt(view, before, byteFixtureInbox, now)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &result)
	})
	if err != nil || !model.IsCanonicalUUID(result.Request) {
		t.Fatal("could not audit both persisted messages", err)
	}
	return result.Request
}

func assertPluginJournalBytes(t *testing.T, name string, raw []byte) {
	t.Helper()
	// Captured with production files at 149740f before the journal boundary.
	// installation: captured again when receive replaced inbox_from/inbox_read,
	// and all but identity when scope, access and the hash revision replaced
	// sessions and capabilities (slice 2).
	want := map[string]string{
		"installation":  "c43a9777df1317e57fafae777c3ed0250036e6bba2f640de709e95036cfeed2b",
		"identity hint": "6427652d690a10e557e0354e70a402d50ebe7180f4bbc03bb7bc0848ff2e235b",
		"inbox index":   "180098f511cc8c376cc5b981658f6a5840912b193a816f34ab075521d587dcf4",
		"native audit":  "d7ab481971eb2351f79ca1d2b1de3d08a8295d15773d9e9fe903e00565478828",
		"inbox audit":   "2de0e921b43c0253a738cee5fe6baad0ada5ea248f1f296303f0490f0f561948",
	}[name]
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want {
		t.Fatalf("%s bytes changed: SHA256 %s, want %s", name, got, want)
	}
}
