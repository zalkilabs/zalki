package bus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Called under the journal lock, after retention. Dry-run also accounts for
// records retention would remove, so its deletion list matches an applied run.
func (r *pruneRun) deleteOrphanFlood() error {
	s, dryRun, report := r.store, r.dryRun, &r.report
	dir := filepath.Join(s.Dir, "flood")
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
		base := entry.Name()
		if !strings.HasSuffix(base, ".json") {
			continue
		}
		kind, native, ok := strings.Cut(strings.TrimSuffix(base, ".json"), "_")
		canonical, err := UUID(native)
		if !ok || !kindPattern.MatchString(kind) || err != nil || canonical != native {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		// Validated canonical names are safe paths, even for an adapter that
		// is no longer installed. Quota contents need not be readable or valid.
		id := kind + ":" + native
		if !r.deleted[id] {
			if _, err := os.Lstat(filepath.Join(s.Dir, "sessions", base)); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		report.DeletedFlood = append(report.DeletedFlood, base)
		if !dryRun {
			if err := os.Remove(filepath.Join(dir, base)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
