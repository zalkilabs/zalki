package bus

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func writeTestWatch(t *testing.T, store Store, watcher, target string, expires time.Time) string {
	t.Helper()
	path, err := store.watchPath(watcher, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveWatch(Watch{Watcher: watcher, Target: target, ExpiresAt: expires.UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneDeletesStaleWatchesWithMatchingDryRun(t *testing.T) {
	store := fixtureStore(t)
	watcher, _, live, _ := registerPair(t, store, "codex", "cursor")
	expiredTarget, _, orphan, _ := registerPair(t, store, "cursor", "cursor")
	old, _, other, _ := registerPair(t, store, "cursor", "claude")
	future := time.Now().Add(time.Hour)

	keep := writeTestWatch(t, store, watcher.ID, live.ID, future)
	keepOther := writeTestWatch(t, store, other.ID, live.ID, future)
	expired := writeTestWatch(t, store, watcher.ID, expiredTarget.ID, time.Now().Add(-time.Minute))
	orphaned := writeTestWatch(t, store, watcher.ID, orphan.ID, future)
	retained := writeTestWatch(t, store, watcher.ID, old.ID, future)

	// An orphan directory that is already empty, and one with an unrelated file.
	emptyDir := filepath.Join(store.Dir, "watches", "cursor_"+newID())
	if err := os.Mkdir(emptyDir, 0700); err != nil {
		t.Fatal(err)
	}
	strangerDir := filepath.Join(store.Dir, "watches", "cursor_"+newID())
	if err := os.Mkdir(strangerDir, 0700); err != nil {
		t.Fatal(err)
	}
	stranger := filepath.Join(strangerDir, "notes.txt")
	if err := os.WriteFile(stranger, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	// A former adapter's watch has a valid canonical name but no session record.
	formerDir := "removed-client_" + newID()
	if err := os.Mkdir(filepath.Join(store.Dir, "watches", formerDir), 0700); err != nil {
		t.Fatal(err)
	}
	former := filepath.Join(store.Dir, "watches", formerDir, "codex_"+newID()+".json")
	if err := os.WriteFile(former, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(store.sessionPathMust(t, orphan.ID)); err != nil {
		t.Fatal(err)
	}
	old.Retired, old.State = true, model.StateRetired
	old.UpdatedAt = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := store.saveSession(old); err != nil {
		t.Fatal(err)
	}

	rel := func(p string) string {
		r, err := filepath.Rel(filepath.Join(store.Dir, "watches"), p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	want := []string{
		rel(expired), rel(filepath.Dir(expired)) + "/",
		rel(orphaned), rel(filepath.Dir(orphaned)) + "/",
		rel(retained), rel(filepath.Dir(retained)) + "/",
		rel(former), formerDir + "/",
		rel(emptyDir) + "/",
	}
	slices.Sort(want)

	preview, err := store.Prune(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Clone(preview.DeletedWatches)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("dry-run watches:\n got %v\nwant %v", got, want)
	}
	for _, path := range []string{keep, keepOther, expired, orphaned, retained, former, emptyDir, stranger} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("dry-run removed %s: %v", path, err)
		}
	}

	report, err := store.Prune(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.DeletedWatches, preview.DeletedWatches) {
		t.Fatalf("applied run differs from dry-run:\n got %v\nwant %v", report.DeletedWatches, preview.DeletedWatches)
	}
	for _, path := range []string{expired, filepath.Dir(expired), orphaned, filepath.Dir(orphaned), retained, filepath.Dir(retained), former, filepath.Dir(former), emptyDir} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("stale watch path remained: %s %v", path, err)
		}
	}
	for _, path := range []string{keep, keepOther, stranger} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("applied run removed %s: %v", path, err)
		}
	}
	again, err := store.Prune(false, nil)
	if err != nil || len(again.DeletedWatches) != 0 {
		t.Fatalf("second prune: %+v %v", again, err)
	}
}

func TestPruneRemovesInvalidWatchRecords(t *testing.T) {
	store := fixtureStore(t)
	watcher, _, target, _ := registerPair(t, store, "codex", "cursor")
	mismatch, _ := register(t, store, "claude")
	bad := writeTestWatch(t, store, watcher.ID, target.ID, time.Now().Add(time.Hour))
	if err := os.WriteFile(bad, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	// A record filed under one target's directory that names another target.
	wrong := mustWatchPath(t, store, watcher.ID, mismatch.ID)
	if err := os.MkdirAll(filepath.Dir(wrong), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(wrong, Watch{Schema: recordSchema, Watcher: watcher.ID, Target: target.ID, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	report, err := store.Prune(false, nil)
	if err != nil || len(report.DeletedWatches) != 4 {
		t.Fatalf("report: %+v %v", report, err)
	}
	for _, path := range []string{bad, wrong} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("invalid watch remained: %s %v", path, err)
		}
	}
}

func mustWatchPath(t *testing.T, store Store, watcher, target string) string {
	t.Helper()
	path, err := store.watchPath(watcher, target)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneDoesNotFollowWatchSymlinks(t *testing.T) {
	store := fixtureStore(t)
	watcher, _, live, _ := registerPair(t, store, "codex", "cursor")
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.json")
	if err := os.WriteFile(victim, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	victimDir := filepath.Join(outside, "dir")
	if err := os.Mkdir(victimDir, 0700); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(victimDir, "codex_"+newID()+".json")
	if err := os.WriteFile(inner, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	// An orphan target directory (no session) containing a symlinked file, and an
	// orphan target that is itself a symlink to a directory with a valid-named file.
	// A symlinked watch under a live target is not read or removed either.
	liveDir := filepath.Dir(writeTestWatch(t, store, watcher.ID, live.ID, time.Now().Add(time.Hour)))
	orphanDir := filepath.Join(store.Dir, "watches", "cursor_"+newID())
	if err := os.Mkdir(orphanDir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(orphanDir, "codex_"+newID()+".json")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(store.Dir, "watches", "cursor_"+newID())
	if err := os.Symlink(victimDir, dirLink); err != nil {
		t.Fatal(err)
	}
	liveLink := filepath.Join(liveDir, "claude_"+newID()+".json")
	if err := os.Symlink(victim, liveLink); err != nil {
		t.Fatal(err)
	}

	for _, dry := range []bool{true, false} {
		report, err := store.Prune(dry, nil)
		if err != nil || len(report.DeletedWatches) != 0 {
			t.Fatalf("dry=%v report: %+v %v", dry, report, err)
		}
	}
	for _, path := range []string{victim, inner, link, dirLink, liveLink} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("symlink case touched %s: %v", path, err)
		}
	}
}

func TestPruneIgnoresWatchNamesThatAreNotCanonical(t *testing.T) {
	store := fixtureStore(t)
	for _, name := range []string{"cursor_notauuid", "Cursor_" + newID(), "cursor_" + newID() + ".json"} {
		dir := filepath.Join(store.Dir, "watches", name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.Prune(false, nil)
	if err != nil || len(report.DeletedWatches) != 0 {
		t.Fatalf("report: %+v %v", report, err)
	}
}

func TestPruneKeepsNewerSchemaWatchesOnLiveAndMissingTargets(t *testing.T) {
	store := fixtureStore(t)
	watcher, _, live, _ := registerPair(t, store, "codex", "cursor")
	orphan, _ := register(t, store, "cursor")
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	write := func(target string) string {
		t.Helper()
		path := mustWatchPath(t, store, watcher.ID, target)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(path, Watch{Schema: recordSchema + 1, Watcher: watcher.ID, Target: target, ExpiresAt: future}); err != nil {
			t.Fatal(err)
		}
		return path
	}
	kept, dropped := write(live.ID), write(orphan.ID)
	if err := os.Remove(store.sessionPathMust(t, orphan.ID)); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		report, err := store.Prune(dry, nil)
		if err != nil || len(report.DeletedWatches) != 0 {
			t.Fatalf("dry=%v report: %+v %v", dry, report, err)
		}
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("dry=%v removed a newer-schema watch on a live target: %v", dry, err)
		}
	}
	if _, err := os.Lstat(dropped); err != nil {
		t.Fatalf("missing target's newer-schema watch was removed: %v", err)
	}
}
