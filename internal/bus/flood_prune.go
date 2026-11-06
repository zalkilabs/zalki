package bus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Called under the journal lock, after retention. Dry-run also accounts for
// records retention would remove, so its deletion list matches an applied run.
// It works in this order: a missing flood directory is fine, one that is not
// private is refused, then each entry is judged by deleteFloodEntry.
func (r *pruneRun) deleteOrphanFlood() error {
	dir := filepath.Join(r.store.Dir, "flood")
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// Reject a symlink or non-private directory before reading or deleting.
	if err := privateDir(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := r.deleteFloodEntry(dir, entry); err != nil {
			return err
		}
	}
	return nil
}

// deleteFloodEntry lists, and unless this is a dry run removes, one quota
// record that has no owner. A name that is not <kind>_<canonical uuid>.json,
// and anything that is not a regular file, is left alone.
func (r *pruneRun) deleteFloodEntry(dir string, entry os.DirEntry) error {
	base := entry.Name()
	target, ok := floodRecordTarget(base)
	if !ok {
		return nil
	}
	info, err := entry.Info()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	orphan, err := r.floodRecordIsOrphan(dir, base, target)
	if err != nil || !orphan {
		return err
	}
	r.report.DeletedFlood = append(r.report.DeletedFlood, base)
	if r.dryRun {
		return nil
	}
	if err := os.Remove(filepath.Join(dir, base)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// floodRecordTarget returns the session ID a quota file name belongs to. The
// name must end in .json and be <kind>_<canonical uuid>.json. Validated
// canonical names are safe paths, even for an adapter that is no longer
// installed. Quota contents need not be readable or valid.
func floodRecordTarget(base string) (string, bool) {
	if !strings.HasSuffix(base, ".json") {
		return "", false
	}
	kind, native, ok := strings.Cut(strings.TrimSuffix(base, ".json"), "_")
	canonical, err := UUID(native)
	if !ok || !validKind(kind) || err != nil || canonical != native {
		return "", false
	}
	return kind + ":" + native, true
}

// floodRecordIsOrphan reports whether a quota record can be removed: its
// session is gone (or this run deletes it) and the record is not from a newer
// format. A session lookup that fails for another reason is an error.
func (r *pruneRun) floodRecordIsOrphan(dir, base, target string) (bool, error) {
	if !r.deleted[target] {
		if _, err := os.Lstat(filepath.Join(r.store.Dir, "sessions", base)); err == nil {
			return false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return !futureRecord(filepath.Join(dir, base), recordSchema), nil
}
