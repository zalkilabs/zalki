package bus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// deleteStaleWatches removes completion watches that can never fire: expired,
// unreadable or invalid records, and records for a target whose session record is
// absent or deleted by retention in this run. A per-target directory left empty
// is removed too. Called under the journal lock, after retention. Dry-run uses
// the same decisions, so its list matches an applied run.
//
// Directory and file names are validated as canonical kind_UUID before they are
// used as paths, so the phase also covers an adapter that is no longer installed.
// Only regular files and real directories are touched; a symlink is never read,
// followed or removed. Names that do not validate are left alone. A watch for a
// live, unexpired target is kept even when its watcher session is gone.
func (r *pruneRun) deleteStaleWatches() error {
	root := filepath.Join(r.store.Dir, "watches")
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// Reject a symlink or non-private directory before reading or deleting.
	if err := privateDir(root); err != nil {
		return err
	}
	targets, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range targets {
		id, ok := watchNameID(entry.Name())
		if !ok || !entry.Type().IsDir() {
			continue
		}
		if err := r.pruneTargetWatches(root, entry.Name(), id); err != nil {
			return err
		}
	}
	return nil
}

// watchNameID returns the session ID that a watches/ path component names.
// Only the canonical form written by watchPath is accepted.
func watchNameID(name string) (string, bool) {
	kind, native, ok := strings.Cut(name, "_")
	canonical, err := UUID(native)
	if !ok || !model.ValidKind(kind) || err != nil || canonical != native {
		return "", false
	}
	return kind + ":" + native, true
}

func (r *pruneRun) pruneTargetWatches(root, dirName, target string) error {
	dir := filepath.Join(root, dirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	orphan := r.deleted[target]
	if !orphan {
		if _, err := os.Lstat(filepath.Join(r.store.Dir, "sessions", dirName+".json")); errors.Is(err, os.ErrNotExist) {
			orphan = true
		} else if err != nil {
			return err
		}
	}
	remaining := 0
	for _, entry := range entries {
		name := entry.Name()
		if _, ok := watchNameID(strings.TrimSuffix(name, ".json")); !ok || !strings.HasSuffix(name, ".json") || !entry.Type().IsRegular() {
			remaining++
			continue
		}
		if futureRecord(filepath.Join(dir, name), recordSchema) || (!orphan && !watchIsStale(filepath.Join(dir, name), target, r.now)) {
			remaining++
			continue
		}
		r.report.DeletedWatches = append(r.report.DeletedWatches, dirName+"/"+name)
		if !r.dryRun {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if remaining > 0 {
		return nil
	}
	r.report.DeletedWatches = append(r.report.DeletedWatches, dirName+"/")
	if r.dryRun {
		return nil
	}
	// Not recursive: a directory that gained an entry is kept.
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// watchIsStale mirrors consumeWatches: a watch that cannot be read, has an
// unparsable or past expiry, or names a different target would be discarded
// there. Unlike consume it never returns an error for a bad record, because
// prune is the cleanup path for them. A record with a newer schema is kept: it
// was written by a newer build for a live target and is not corrupt.
func watchIsStale(path, target string, now time.Time) bool {
	var watch Watch
	if err := readRecord(path, &watch, &watch.Schema); err != nil {
		return ErrorCodeOf(err) != CodeUnsupportedSchema
	}
	expires, err := time.Parse(time.RFC3339Nano, watch.ExpiresAt)
	return err != nil || !expires.After(now) || watch.Target != target
}
