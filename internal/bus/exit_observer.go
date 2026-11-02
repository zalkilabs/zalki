package bus

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DefaultSweepInterval is the fallback sweep period. Kernel notifications make
// notices for process-owned sessions immediate; the sweep covers sessions whose
// liveness is not a process (endpoint-based), notification failures and anything
// missed.
const DefaultSweepInterval = 5 * time.Minute

// observerStampName is touched by whichever server sweeps, so servers share one
// sweep per half interval instead of each sweeping on its own schedule.
const observerStampName = "watch-observer.stamp"

// dirSettle coalesces a burst of directory events into one re-read.
const dirSettle = 50 * time.Millisecond

// ObserveExits delivers ended notices for watched sessions that exited without
// a SessionEnd hook (a kill or crash), until ctx is cancelled. Run it in its own
// goroutine; it never blocks the caller's tool calls. Failures are logged and
// skipped, and a panic stops only the observer.
//
// It keeps no timer faster than the sweep. Idle, with no watches, it is blocked
// on the kernel; with the platform's exit source unavailable it falls back to the
// sweep alone.
func (s *Service) ObserveExits(ctx context.Context, sweep time.Duration) {
	s.observeExits(ctx, sweep, newExitSource)
}

func (s *Service) observeExits(ctx context.Context, sweep time.Duration, open func() (exitSource, error)) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.Store.logEvent(slog.LevelError, "watch.observer_panic", eventData{Err: errors.New("exit observer panicked")})
		}
	}()
	o := &exitObserver{
		service: s, root: filepath.Join(s.Store.Dir, "watches"),
		procs: map[processKey]ProcessRef{}, owners: map[processKey][]string{},
	}
	// Ensure the directory exists so its creation of entries is observable.
	if err := privateDir(o.root); err != nil {
		s.Store.logEvent(slog.LevelDebug, "watch.observer_skipped", eventData{Err: err})
		o.rootUnusable = true
	}
	var events <-chan exitEvent
	if source, err := open(); err != nil {
		s.Store.logEvent(slog.LevelDebug, "watch.observer_sweep_only", eventData{Err: err})
	} else {
		defer source.Close() //nolint:errcheck // Nothing to recover at shutdown.
		o.source, events = source, source.Events()
	}
	settle := time.NewTimer(time.Hour)
	settle.Stop()
	sweepTimer := time.NewTimer(jittered(sweep))
	defer sweepTimer.Stop()
	settling := false

	o.sync(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-events:
			switch event.Kind {
			case exitProcess:
				o.processExited(ctx, event.Process)
			case exitDirChanged:
				if !settling {
					settling = true
					settle.Reset(dirSettle)
				}
			case exitSourceFailed:
				s.Store.logEvent(slog.LevelWarn, "watch.observer_source_failed", eventData{Err: errors.New("exit notification source failed")})
				o.source, events = nil, nil
			}
		case <-settle.C:
			settling = false
			o.sync(ctx)
		case <-sweepTimer.C:
			if s.Store.claimObservation(sweep, time.Now()) {
				o.sync(ctx)
			}
			sweepTimer.Reset(jittered(sweep))
		}
	}
}

// jittered spreads servers started together: interval ±20%.
func jittered(interval time.Duration) time.Duration {
	return time.Duration(float64(interval) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: scheduling jitter, not security.
}

// exitObserver is owned by one goroutine; only the exit source is shared.
type exitObserver struct {
	service      *Service
	root         string
	rootUnusable bool
	source       exitSource // nil in sweep-only mode
	procs        map[processKey]ProcessRef
	owners       map[processKey][]string // watched owner process -> targets it owns
}

// sync re-reads the watched targets without the journal lock, registers their
// owner processes and directories with the exit source, and notices targets that
// already look exited (a sweep, and the start-up check). It skips anything it
// cannot trust: non-canonical names, symlinks, unreadable, future-schema or
// unknown-adapter sessions. Prune cleans those up.
func (o *exitObserver) sync(ctx context.Context) {
	s := o.service
	if o.source != nil && !o.rootUnusable {
		if err := o.source.WatchDir(o.root); err != nil {
			s.Store.logEvent(slog.LevelDebug, "watch.observer_skipped", eventData{Err: err})
		}
	}
	info, err := os.Lstat(o.root)
	if err != nil || !info.IsDir() {
		o.reconcile(map[processKey]ProcessRef{}, map[processKey][]string{})
		return
	}
	entries, err := os.ReadDir(o.root)
	if err != nil {
		return
	}
	wanted, owners := map[processKey]ProcessRef{}, map[processKey][]string{}
	var exited []string
	for _, entry := range entries {
		id, ok := watchNameID(entry.Name())
		if !ok || !entry.Type().IsDir() {
			continue
		}
		dir := filepath.Join(o.root, entry.Name())
		// Register the directory before reading it, so a watch added in between
		// raises an event instead of being missed.
		if o.source != nil {
			if err := o.source.WatchDir(dir); err != nil {
				s.Store.logEvent(slog.LevelDebug, "watch.observer_skipped", eventData{Actor: id, Err: err})
			}
		}
		if files, err := os.ReadDir(dir); err != nil || len(files) == 0 {
			continue
		}
		v, err := s.Store.session(id)
		if err != nil {
			s.Store.logEvent(slog.LevelDebug, "watch.observer_skipped", eventData{Actor: id, Err: err})
			continue
		}
		if v.Retired || s.Store.sessionLiveness(v) == LivenessExited {
			exited = append(exited, id)
			continue
		}
		if v.Owner != nil && v.Owner.PID > 1 {
			key := keyOf(*v.Owner)
			wanted[key] = *v.Owner
			owners[key] = append(owners[key], id)
		}
	}
	o.reconcile(wanted, owners)
	for _, id := range exited {
		if ctx.Err() != nil {
			return
		}
		o.notice(ctx, id)
	}
}

// reconcile makes the exit source watch exactly the wanted owner processes.
func (o *exitObserver) reconcile(wanted map[processKey]ProcessRef, owners map[processKey][]string) {
	o.owners = owners
	if o.source == nil {
		return
	}
	for key, ref := range o.procs {
		if _, ok := wanted[key]; !ok {
			o.source.Unwatch(ref)
			delete(o.procs, key)
		}
	}
	for key, ref := range wanted {
		if _, ok := o.procs[key]; ok {
			continue
		}
		if err := o.source.WatchProcess(ref); err != nil {
			// The sweep still covers this target.
			o.service.Store.logEvent(slog.LevelDebug, "watch.observer_skipped", eventData{Err: err})
			continue
		}
		o.procs[key] = ref
	}
}

// processExited handles a kernel exit event for one owner process. The decision
// is still made from the session record under the lock, so an adapter whose
// liveness is not simply "owner alive" keeps its own rule.
func (o *exitObserver) processExited(ctx context.Context, ref ProcessRef) {
	key := keyOf(ref)
	targets := o.owners[key]
	delete(o.owners, key)
	delete(o.procs, key)
	for _, id := range targets {
		o.notice(ctx, id)
	}
}

// notice repeats the exit decision under the journal lock, consumes the due
// watches and delivers their ended notices. Consumption removes the records, so
// several servers observing one exit never deliver twice.
func (o *exitObserver) notice(ctx context.Context, id string) {
	s := o.service
	subject, watches, err := s.consumeIfExited(id)
	if err != nil {
		s.Store.logEvent(slog.LevelDebug, "watch.observer_skipped", eventData{Actor: id, Err: err})
		return
	}
	if len(watches) == 0 {
		return
	}
	s.Store.logEvent(slog.LevelInfo, "watch.exit_observed", eventData{Actor: subject.ID})
	if err := s.deliverEnded(ctx, subject, watches); err != nil {
		s.Store.logEvent(slog.LevelWarn, "watch.observer_delivery_failed", eventData{Actor: subject.ID, Err: err})
	}
}

// consumeIfExited repeats the exit decision on a fresh read under the journal
// lock, so a session that re-registered since the scan is not reported as ended,
// and then consumes its due watches.
func (s *Service) consumeIfExited(id string) (Session, []Watch, error) {
	var subject Session
	var watches []Watch
	err := s.Store.locked(func() error {
		v, err := s.Store.session(id)
		if err != nil {
			return err
		}
		if !v.Retired && s.Store.sessionLiveness(v) != LivenessExited {
			return nil
		}
		subject = v
		watches, err = s.Store.consumeWatchesLocked(id, time.Now())
		return err
	})
	return subject, watches, err
}

// claimObservation reports whether this server should sweep now. It is false
// only when another server swept within half an interval. An unusable stamp means
// sweep anyway: the stamp only saves work.
func (s Store) claimObservation(interval time.Duration, now time.Time) bool {
	path := filepath.Join(s.Dir, observerStampName)
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		if age := now.Sub(info.ModTime()); age >= 0 && age < interval/2 {
			return false
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0600) //nolint:gosec // G304: fixed name in the private state directory.
	if err != nil {
		return true
	}
	_ = file.Close()
	_ = os.Chtimes(path, now, now)
	return true
}
