package bus

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestPruneDeletesOrphanFloodWithMatchingDryRun(t *testing.T) {
	store := fixtureStore(t)
	sender, _, live, _ := registerPair(t, store, "codex", "cursor")
	young, _, old, _ := registerPair(t, store, "cursor", "cursor")
	orphan, _ := register(t, store, "cursor")
	paths := map[string]string{}
	before := map[string]string{}
	for _, v := range []Session{live, young, old, orphan} {
		if err := spendFlood(store, Message{ID: newID(), From: sender.ID, To: v.ID}, time.Now()); err != nil {
			t.Fatal(err)
		}
		path, _ := store.floodPath(v.ID)
		paths[v.ID] = path
		before[v.ID] = string(floodBytes(t, store, v.ID))
	}
	young.Retired, young.State = true, StateRetired
	old.Retired, old.State = true, StateRetired
	old.UpdatedAt = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	for _, v := range []Session{young, old} {
		if err := store.saveSession(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(store.sessionPathMust(t, orphan.ID)); err != nil {
		t.Fatal(err)
	}
	// Orphan cleanup uses validated file names, not quota JSON. Corrupt quota
	// for an existing target must be preserved rather than reset by deletion.
	for _, v := range []Session{orphan, live} {
		if err := os.WriteFile(paths[v.ID], []byte("invalid quota"), 0600); err != nil {
			t.Fatal(err)
		}
		before[v.ID] = "invalid quota"
	}
	formerAdapter := "removed-client_" + newID() + ".json"
	formerPath := filepath.Join(store.Dir, "flood", formerAdapter)
	if err := os.WriteFile(formerPath, []byte("old quota"), 0600); err != nil {
		t.Fatal(err)
	}
	history := Message{ID: newID(), From: sender.ID, To: old.ID, Body: "keep history", Status: StatusSubmitted}
	if err := store.saveMessage(history); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Base(paths[old.ID]), filepath.Base(paths[orphan.ID]), formerAdapter}
	slices.Sort(want)
	preview, err := store.Prune(true, nil)
	if err != nil || !slices.Equal(preview.DeletedFlood, want) || len(preview.DeletedSessions) != 1 {
		t.Fatalf("dry-run: %+v %v", preview, err)
	}
	for id, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != before[id] {
			t.Fatalf("dry-run changed quota for %s: %v", id, err)
		}
	}
	if _, err := os.Stat(formerPath); err != nil {
		t.Fatal("dry-run removed a former adapter's quota", err)
	}
	if _, err := os.Stat(store.sessionPathMust(t, old.ID)); err != nil {
		t.Fatal("dry-run removed the old session", err)
	}
	report, err := store.Prune(false, nil)
	if err != nil || !slices.Equal(report.DeletedFlood, want) {
		t.Fatalf("applied prune differs from dry-run: %+v %v", report, err)
	}
	for _, path := range []string{paths[old.ID], paths[orphan.ID], formerPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan quota remained: %s %v", path, err)
		}
	}
	for _, id := range []string{live.ID, young.ID} {
		content, err := os.ReadFile(paths[id])
		if err != nil || string(content) != before[id] {
			t.Fatal("prune removed or rewrote quota for a retained session", err)
		}
	}
	if msg, err := store.message(history.ID); err != nil || msg.Body != history.Body {
		t.Fatal("quota cleanup removed message history", err)
	}
	again, err := store.Prune(false, nil)
	if err != nil || len(again.DeletedFlood) != 0 {
		t.Fatalf("repeated prune was not idempotent: %+v %v", again, err)
	}
}

func TestPruneFloodLeavesUnrelatedFilesAndSymlinks(t *testing.T) {
	store := fixtureStore(t)
	if err := store.locked(func() error { return privateDir(filepath.Join(store.Dir, "flood")) }); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.Dir, "flood")
	for _, base := range []string{"notes.json", "cursor_not-a-uuid.json"} {
		if err := os.WriteFile(filepath.Join(dir, base), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	folder := filepath.Join(dir, "cursor_"+newID()+".json")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(external, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "cursor_"+newID()+".json")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		report, err := store.Prune(dry, nil)
		if err != nil || len(report.DeletedFlood) != 0 {
			t.Fatalf("prune selected unrelated files: %+v %v", report, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 4 {
		t.Fatal("prune removed an unrelated file, directory or symlink", err)
	}
	if content, err := os.ReadFile(external); err != nil || string(content) != "keep" {
		t.Fatal("prune followed a symlink", err)
	}
}
