package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPrunePreservesQuietAndUnknownOwnersAndMessageJournal(t *testing.T) {
	p := selfProcess(t)
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	dead, _ := register(t, store, "cursor")
	quiet, _ := register(t, store, "cursor")
	unknown, _ := register(t, store, "cursor")
	msg, err := (&Service{Store: store}).Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: dead.ID, MessageID: newID(), Body: "retain history"})
	if err != nil {
		t.Fatal(err)
	}
	dead.Owner = &ProcessRef{PID: p.PID, Start: "previous-process-at-this-pid", Role: "client"}
	quiet.Owner = &ProcessRef{PID: p.PID, Start: p.Start, Role: "backend"}
	quiet.UpdatedAt = "2000-01-01T00:00:00.000000000Z"
	for _, v := range []Session{dead, quiet} {
		if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(store.Dir, "messages", msg.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.Prune(true, nil)
	if err != nil || len(report.Sessions) != 1 || report.Sessions[0].ID != dead.ID {
		t.Fatalf("bad preview: %+v %v", report, err)
	}
	stored, _ := store.session(dead.ID)
	if stored.Retired {
		t.Fatal("dry-run changed registration")
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, v := range listed {
		seen[v.ID] = true
	}
	if seen[dead.ID] || !seen[quiet.ID] || !seen[unknown.ID] {
		t.Fatal("discovery guessed liveness or included an exited owner")
	}
	if _, err := (&Service{Store: store}).Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: dead.ID, MessageID: newID(), Body: "must not deliver"}); err == nil {
		t.Fatal("sent to exited owner before prune")
	}
	report, err = store.Prune(false, nil)
	if err != nil || len(report.Sessions) != 1 {
		t.Fatalf("prune failed: %+v %v", report, err)
	}
	stored, _ = store.session(dead.ID)
	if !stored.Retired || stored.RetiredReason != "owner_exited" {
		t.Fatal("prune did not record exit reason")
	}
	after, _ := os.ReadFile(filepath.Join(store.Dir, "messages", msg.ID+".json"))
	if string(before) != string(after) {
		t.Fatal("cleanup changed historical messages or receipts")
	}
	if repeat, err := store.Prune(false, nil); err != nil || len(repeat.Sessions) != 0 {
		t.Fatal("prune was not idempotent", err)
	}
}

func TestExplicitPruneValidatesAllSelections(t *testing.T) {
	store := fixtureStore(t)
	known, _ := register(t, store, "cursor")
	if _, err := store.Prune(false, []string{known.ID, "cursor:" + newID()}); err == nil {
		t.Fatal("accepted nonexistent selection")
	}
	v, _ := store.session(known.ID)
	if v.Retired {
		t.Fatal("partially applied invalid selections")
	}
	if _, err := store.Prune(false, []string{"cursor:reviewer"}); err == nil {
		t.Fatal("cleanup accepted a name instead of exact identity")
	}
	if report, err := store.Prune(false, []string{known.ID}); err != nil || len(report.Sessions) != 1 || report.Sessions[0].Reason != "explicit_cleanup" {
		t.Fatalf("explicit cleanup failed: %+v %v", report, err)
	}
}

func TestRuntimeHealthDetectsAtomicBinaryReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zalki")
	if err := os.WriteFile(path, []byte("old executable"), 0700); err != nil {
		t.Fatal(err)
	}
	old, err := binaryDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	info := RuntimeInfo{Binary: path, LoadedBuild: old}
	if h := runtimeHealth(info); h.NeedsRestart || h.DiskBuild != old {
		t.Fatal("unchanged binary marked outdated")
	}
	replacement := path + ".new"
	if err := os.WriteFile(replacement, []byte("new executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	h := runtimeHealth(info)
	if !h.NeedsRestart || h.LoadedBuild != old || h.DiskBuild == old {
		t.Fatal("atomic replacement relabeled the loaded build")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if h := runtimeHealth(info); h.BuildError == "" || h.LoadedBuild != old {
		t.Fatal("missing installed binary lost the loaded fingerprint")
	}
}

func TestRuntimeTrackingCleanupAndNoCredentialLeak(t *testing.T) {
	store := fixtureStore(t)
	actor, key := register(t, store, "cursor")
	lease, err := store.TrackRuntime("mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	lease.ObserveActor(actor.ID)
	lease.ObserveActor(actor.ID)
	if info := lease.Info(); len(info.Actors) != 1 || info.LoadedBuild == "" {
		t.Fatal("runtime actor/build metadata missing")
	}
	report, err := store.Diagnostics()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(report)
	if strings.Contains(string(b), key) || strings.Contains(string(b), actor.KeyHash) || strings.Contains(string(b), "key_hash") {
		t.Fatal("doctor exposed credentials")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); lease.ObserveActor(actor.ID) }()
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	lease.ObserveActor("cursor:" + newID())
	files, _ := filepath.Glob(filepath.Join(store.Dir, "runtimes", "*.json"))
	if len(files) != 0 {
		t.Fatal("closed runtime record was resurrected")
	}
}

func TestPruneRemovesOnlyExitedRuntimeRecords(t *testing.T) {
	p := selfProcess(t)
	store := fixtureStore(t)
	lease, err := store.TrackRuntime("mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	stale := lease.Info()
	stale.ID = newID()
	stale.Process = &ProcessRef{PID: p.PID, Start: "previous-process", Role: "zalki"}
	if err := store.locked(func() error { return writeJSON(filepath.Join(store.Dir, "runtimes", stale.ID+".json"), stale) }); err != nil {
		t.Fatal(err)
	}
	report, err := store.Prune(false, nil)
	if err != nil || len(report.Runtimes) != 1 || report.Runtimes[0] != stale.ID {
		t.Fatalf("wrong runtime cleanup: %+v %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "runtimes", lease.info.ID+".json")); err != nil {
		t.Fatal("removed a running runtime")
	}
}

func TestCursorResumedPromptReplacesExitedOwnerEvidence(t *testing.T) {
	p := selfProcess(t)
	store := fixtureStore(t)
	v, key := register(t, store, "cursor")
	v.Owner = &ProcessRef{PID: p.PID, Start: "old-client-at-reused-pid", Role: "client"}
	v.Retired, v.State, v.RetiredReason = true, "retired", "owner_exited"
	if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"conversation_id": strings.TrimPrefix(v.ID, "cursor:"), "hook_event_name": "beforeSubmitPrompt"})
	var out bytes.Buffer
	service := Service{Store: store}
	if err := service.Hook("cursor", bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.authenticate(v.ID, key)
	if err != nil || resumed.Retired || resumed.RetiredReason != "" || resumed.Generation != v.Generation || processLiveness(resumed.Owner) == "exited" {
		t.Fatalf("actual prompt left stale owner evidence sticky: %+v %v", resumed.Owner, err)
	}
}
