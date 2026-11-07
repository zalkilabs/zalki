package bus

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

type PruneItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type PruneReport struct {
	DryRun          bool        `json:"dry_run"`
	Sessions        []PruneItem `json:"sessions"`
	Runtimes        []string    `json:"runtimes"`
	DeletedSessions []PruneItem `json:"deleted_sessions"`
	DeletedLocks    []string    `json:"deleted_locks"`
	DeletedFlood    []string    `json:"deleted_flood"`
	// DeletedWatches lists watches/ paths: target_dir/watcher.json files and
	// "target_dir/" for a per-target directory removed once empty.
	DeletedWatches []string `json:"deleted_watches"`
}

// DefaultRetiredRetention is how long a retired session record is kept.
// Message history is not part of this window.
const DefaultRetiredRetention = 7 * 24 * time.Hour

// pruneRun holds cleanup decisions separately from the human-readable report.
// All phases run under the journal lock; dry runs use the same typed decisions.
type pruneRun struct {
	store     Store
	dryRun    bool
	report    PruneReport
	selected  map[string]bool
	retired   map[string]bool
	deleted   map[string]bool
	now       time.Time
	retention time.Duration
}

// Automatic cleanup requires positive exit evidence, never age or lack of mail.
// Optional exact IDs allow the human to retire legacy records with no owner
// proof; no named/prefix match and no process termination is performed.
// Age only deletes records that are already retired. Prune stays a command:
// running it from a session hook would scan and delete while a client is starting.
func (s Store) Prune(dryRun bool, selected []string) (PruneReport, error) {
	return s.PruneRetired(dryRun, selected, DefaultRetiredRetention)
}

// PruneRetired retires exited sessions and deletes retired records older than
// retiredOlderThan. Messages are kept. A negative window is rejected.
func (s Store) PruneRetired(dryRun bool, selected []string, retiredOlderThan time.Duration) (PruneReport, error) {
	run := pruneRun{
		store: s, dryRun: dryRun, retention: retiredOlderThan,
		report:   PruneReport{DryRun: dryRun, Sessions: []PruneItem{}, Runtimes: []string{}, DeletedSessions: []PruneItem{}, DeletedLocks: []string{}, DeletedFlood: []string{}, DeletedWatches: []string{}},
		selected: map[string]bool{}, retired: map[string]bool{}, deleted: map[string]bool{},
	}
	if retiredOlderThan < 0 {
		return run.report, errors.New("retired retention window must not be negative")
	}
	for _, id := range selected {
		if _, err := s.sessionRecordPath(id); err != nil {
			return run.report, err
		}
		run.selected[id] = true
	}
	var err error
	if dryRun {
		err = s.readLocked(run.execute)
	} else {
		err = s.locked(run.execute)
	}
	return run.report, err
}

func (r *pruneRun) execute() error {
	// Validate every selection and read both snapshots before any mutation.
	for id := range r.selected {
		path, err := r.store.sessionRecordPath(id)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(path); err != nil {
			return err
		}
	}
	sessions, issues, err := r.store.scanSessions()
	if err != nil {
		return err
	}
	runtimes, err := r.store.rawRuntimes()
	if err != nil {
		return err
	}
	for _, issue := range issues {
		r.report.Sessions = append(r.report.Sessions, PruneItem(issue))
	}
	r.now = time.Now()
	if err := r.sweepSessions(sessions); err != nil {
		return err
	}
	if err := r.store.reconcileApprovalHolds(r.dryRun, &r.report, r.retired); err != nil {
		return err
	}
	if err := r.deleteOrphanListenerLocks(); err != nil {
		return err
	}
	if err := r.deleteOrphanFlood(); err != nil {
		return err
	}
	if err := r.deleteStaleWatches(); err != nil {
		return err
	}
	return r.deleteExitedRuntimes(runtimes)
}

// sweepSessions handles each session in ID order: retired records are deleted
// or have a leftover lease released, and live sessions are retired when they
// were selected or their owner exited.
func (r *pruneRun) sweepSessions(sessions []Session) error {
	for _, v := range sessions {
		var err error
		if v.Retired {
			err = r.sweepRetired(v)
		} else {
			err = r.retireIfDue(v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// sweepRetired deletes a retired record that is old enough. Otherwise, old
// retired records can retain a lease: it is released without changing the
// retirement. Releasing sets UpdatedAt to now, which also restarts the
// retention window (characterized, not endorsed).
func (r *pruneRun) sweepRetired(v Session) error {
	if model.RetiredLongEnough(v.UpdatedAt, r.now, r.retention) {
		return r.deleteRetired(v)
	}
	if v.Listener == nil {
		return nil
	}
	r.report.Sessions = append(r.report.Sessions, PruneItem{v.ID, "lease_released"})
	if r.dryRun {
		return nil
	}
	releaseListenerLease(&v)
	v.UpdatedAt = model.Timestamp()
	return r.store.saveSession(v)
}

// retireIfDue retires a live session that was selected or whose owner exited.
// The decision is reported before the record is saved; a dry run stops there.
func (r *pruneRun) retireIfDue(v Session) error {
	selected := r.selected[v.ID]
	liveness := LivenessUnknown
	if !selected {
		liveness = r.store.sessionLiveness(v)
	}
	reason := model.PruneRetirementReason(selected, liveness)
	if reason == "" {
		return nil
	}
	r.retired[v.ID] = true
	r.report.Sessions = append(r.report.Sessions, PruneItem{v.ID, reason})
	if r.dryRun {
		return nil
	}
	retireSession(&v, reason)
	return r.store.saveSession(v)
}

func (r *pruneRun) deleteExitedRuntimes(runtimes []RuntimeInfo) error {
	for _, v := range runtimes {
		if processLiveness(v.Process) != "exited" {
			continue
		}
		r.report.Runtimes = append(r.report.Runtimes, v.ID)
		if !r.dryRun {
			if err := os.Remove(filepath.Join(r.store.Dir, "runtimes", v.ID+".json")); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *pruneRun) deleteRetired(v Session) error {
	s, dryRun, report := r.store, r.dryRun, &r.report
	r.deleted[v.ID] = true
	report.DeletedSessions = append(report.DeletedSessions, PruneItem{v.ID, "retention"})
	path, err := s.sessionPath(v.ID)
	if err != nil {
		return err
	}
	lockRemoved, err := s.removeFreeListenerLock(v.Kind, v.ID, !dryRun)
	if err != nil {
		return err
	}
	if lockRemoved {
		report.DeletedLocks = append(report.DeletedLocks, filepath.Base(s.listenerLockPath(v.Kind, v.ID)))
	}
	if dryRun {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r *pruneRun) deleteOrphanListenerLocks() error {
	s, dryRun, report := r.store, r.dryRun, &r.report
	matches, err := filepath.Glob(filepath.Join(s.Dir, "*-listener-*.lock"))
	if err != nil {
		return err
	}
	sort.Strings(matches)
	for _, path := range matches {
		id, ok := sessionIDFromListenerLock(filepath.Base(path))
		if !ok {
			continue
		}
		sessionPath, err := s.sessionRecordPath(id)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(sessionPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		kind, native, _ := strings.Cut(id, ":")
		removed, err := s.removeFreeListenerLock(kind, kind+":"+native, !dryRun)
		if err != nil {
			return err
		}
		if removed {
			report.DeletedLocks = append(report.DeletedLocks, filepath.Base(path))
		}
	}
	return nil
}

func sessionIDFromListenerLock(base string) (string, bool) {
	name := strings.TrimSuffix(base, ".lock")
	kind, native, ok := strings.Cut(name, "-listener-")
	if !ok || kind == "" || native == "" || strings.Contains(kind, "-listener-") {
		return "", false
	}
	return kind + ":" + native, true
}
