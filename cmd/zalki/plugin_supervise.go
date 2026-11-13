package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// reasonText holds every controlled reason code and its owner text. No
// child error text is ever stored or shown in its place.
var reasonText = map[string]string{
	"exited":                "host exited",
	"failed":                "host failed",
	"killed":                "host was killed",
	"start_failed":          "the host could not start",
	"pin_changed":           "the plugin file changed while it started; install it again",
	"pin_invalid":           "the plugin file no longer matches its pin; install it again",
	"unsupported_wire":      "the plugin does not speak the current plugin wire; rebuild it",
	"refused":               "Zalki refused an operation; check its access",
	"lease_held":            "lease held, holder unknown",
	"binding_changed":       "its access or settings changed",
	"disabled":              "disabled",
	"removed":               "removed",
	"record_unreadable":     "its record could not be read",
	"log_failed":            "its log could not be written",
	"supervisor_stopped":    "the supervisor stopped",
	"zalki_replaced":        "Zalki was updated; a new supervisor takes over",
	"restart_requested":     "a restart was requested",
	"prior_run_ended":       "an earlier supervisor's run ended",
	"prior_run_unconfirmed": "an earlier run is not confirmed ended; check it, then restart",
}

// permanentReasons need the owner (repair or reinstall): no automatic retry.
var permanentReasons = map[string]bool{"pin_changed": true, "pin_invalid": true, "unsupported_wire": true, "refused": true}

func validReason(code string) bool {
	_, ok := reasonText[code]
	return code == "" || ok
}

const supervisePoll = 5 * time.Second

func newPluginSuperviseCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "supervise", Short: "Run all enabled plugins and restart them after failures", Args: cobra.NoArgs,
		Long: "Runs in the foreground until stopped. Each enabled plugin runs in its own host process; a failing plugin is restarted with growing waits, and after 5 failures in 10 minutes it stays stopped until you run zalki plugins restart NAME. Output goes only to private logs.",
		// supervise ignores config, logging and state selectors from the
		// environment: it uses only --state, or the default state.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			for _, flag := range []string{"config", "log-level", "log-file"} {
				if cmd.Flags().Changed(flag) {
					return model.NewError(model.CodeInvalidInput, "zalki plugins supervise does not take --%s", flag)
				}
			}
			// No root logger or journal from inherited selectors: the
			// controller writes only its own private log.
			opts.logger = slog.New(slog.DiscardHandler)
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSupervisor(cmd.Context(), opts.state, cmd.Flags().Changed("state"))
		},
	}
}

func runSupervisor(ctx context.Context, explicitState string, stateGiven bool) error {
	home, err := os.UserHomeDir()
	if err == nil {
		err = cleanAbsolute(home)
	}
	if err != nil {
		return model.NewError(model.CodeInvalidInput, "zalki plugins supervise needs a clean absolute HOME")
	}
	state := filepath.Join(home, ".local", "state", "zalki")
	if stateGiven {
		if state, err = filepath.Abs(explicitState); err != nil {
			return err
		}
	}
	binary, err := stableBinary()
	if err != nil {
		return err
	}
	binaryID, err := resolveBinary(binary)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(state, "plugins")); errors.Is(err, os.ErrNotExist) {
		return nil // no plugins: nothing to supervise
	}
	if err := privateDirectory(filepath.Join(state, "plugins")); err != nil {
		if model.ErrorCodeOf(err) == model.CodeConflict {
			// A positively unsafe directory: a restart cannot help, exit 0.
			return errUnsafePluginsDir
		}
		return err // an unknown I/O error: exit 1, a new supervisor retries
	}
	lock, held, err := acquireSupervisorLock(state)
	if err != nil || held {
		return err // another supervisor already runs this state: exit 0
	}
	defer lock.Close() //nolint:errcheck // Held for the controller's lifetime; never unlinked.
	logs, err := openRotatingLog(filepath.Join(state, "plugins", ".supervisor.log"))
	if err != nil {
		return err
	}
	defer logs.close() //nolint:errcheck // Controller log; closed at exit.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	s := &supervisor{state: state, home: home, binary: binary, binaryID: binaryID, resolve: resolveBinary, saveRun: saveRunState, instance: model.NewUUID(), log: logs, now: time.Now, plugins: map[string]*supervised{}, events: make(chan hostEvent, 64)}
	return s.run(ctx)
}

// errUnsafePluginsDir is the permanent setup refusal (exit 0).
var errUnsafePluginsDir = errors.New("the plugins folder is a link or not a private directory of this user; nothing was written")

// privateDirectory accepts only a real directory (no link) of this user
// that others cannot write: nothing is created through anything else.
func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || stat.Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t.
		return model.NewError(model.CodeConflict, "%s is not a private directory of this user; nothing was written", path)
	}
	return nil
}

func cleanAbsolute(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("not a clean absolute path")
	}
	return nil
}

// acquireSupervisorLock takes the singleton lock for this state without
// waiting. held means another supervisor owns it.
func acquireSupervisorLock(state string) (*os.File, bool, error) {
	path := filepath.Join(state, "plugins", ".supervisor.lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // G304: fixed path under the state directory; links refused.
	if err != nil {
		return nil, false, err
	}
	if info, err := file.Stat(); err != nil || !privateFile(info) {
		_ = file.Close()
		return nil, false, unsafeLog(path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return file, false, nil
}

type bindingKey struct{ id, epoch, revision string }

// supervised is one instance under supervision.
type supervised struct {
	run        pluginRunState
	binding    bindingKey
	host       *hostProcess
	lastReason string // the reason in the host's stopped frame
	// unsaved: the current run state is not known to be durable (a failed
	// or uncertain write). No new start happens until a save succeeds, so
	// a counted failure or deadline is never lost before a retry.
	unsaved bool
	// since and retryAt are live monotonic anchors (healthy since, next
	// retry); the persisted UTC times are only for status and recovery.
	since, retryAt time.Time
	// recoverUntil is set while a run of an earlier controller is not yet
	// confirmed ended; no start happens before it is.
	recoverUntil time.Time
}

type supervisor struct {
	state, home, binary, instance string
	binaryID                      binaryIdentity
	saveRun                       func(dir, name string, state pluginRunState) error
	resolve                       func(string) (binaryIdentity, error)
	missingSince                  *time.Time
	log                           *rotatingLog
	now                           func() time.Time
	plugins                       map[string]*supervised
	events                        chan hostEvent
}

// run is one event loop: host events, one 5-second poll, and a wake-up at
// the earliest backoff deadline. Scans never overlap.
func (s *supervisor) run(ctx context.Context) error {
	tick := time.NewTicker(supervisePoll)
	defer tick.Stop()
	retry := time.NewTimer(supervisePoll)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopAll("supervisor_stopped")
			return nil
		case ev := <-s.events:
			s.handle(ev)
			retry.Reset(s.nextRetry())
			continue
		case <-tick.C:
		case <-retry.C:
		}
		switch s.checkBinary() {
		case binaryUnknown:
			s.stopAll("zalki_replaced")
			return errors.New("the zalki executable cannot be checked; stopped so a new supervisor can retry")
		case binarySame:
		case binaryChanged:
			s.stopAll("zalki_replaced")
			return errZalkiReplaced
		case binaryMissing:
			s.stopAll("zalki_replaced") // retried until the grace ends
			continue
		case binaryGone:
			s.stopAll("zalki_replaced")
			return nil // uninstalled: a restart cannot help
		}
		active, err := s.scan()
		if err != nil {
			s.stopAll("supervisor_stopped")
			return err // unknown, not absent: exit 1 so a new supervisor retries
		}
		if !active {
			s.stopAll("supervisor_stopped")
			return nil // positively no enabled plugins: launchd need not restart us
		}
		retry.Reset(s.nextRetry())
	}
}

// nextRetry is the wait until the earliest backoff deadline, at most one poll.
func (s *supervisor) nextRetry() time.Duration {
	wait := supervisePoll
	for _, p := range s.plugins {
		if p.host == nil && p.run.State == runBackoff && !p.retryAt.IsZero() {
			wait = min(wait, max(0, p.retryAt.Sub(s.now())))
		}
	}
	return wait
}

func (s *supervisor) logf(format string, args ...any) {
	_ = s.log.write([]byte(fmt.Sprintf("%s "+format+"\n", append([]any{s.now().UTC().Format(time.RFC3339)}, args...)...)))
}

// pluginNames lists canonical NAME.json files; run, restart and identity
// files never pass as an installation name.
func (s *supervisor) pluginNames() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.state, "plugins"))
	if err != nil {
		s.logf("cannot list plugins: %s", model.ErrorCodeOf(err))
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if ok && entry.Type().IsRegular() && len(name) <= 32 && model.ValidKind(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// scan reconciles every instance with its current record. It reports
// whether anything is enabled, still running or unknown; a listing error
// is returned, never read as "nothing enabled".
func (s *supervisor) scan() (bool, error) {
	names, err := s.pluginNames()
	if err != nil {
		return true, err
	}
	active := false
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
		if s.reconcile(name) {
			active = true
		}
	}
	for name, p := range s.plugins {
		if !seen[name] && p.host != nil {
			s.stopHost(name, p, "removed")
		}
	}
	return active, nil
}

func (s *supervisor) reconcile(name string) bool {
	snap, err := pluginfeed.NewPluginFeed(bus.Store{Dir: s.state}, name).PluginSnapshot()
	p := s.plugins[name]
	if err != nil {
		// Retried at the next poll; never treated as disabled or enabled.
		if p != nil && p.host != nil {
			s.stopHost(name, p, "record_unreadable")
		}
		return true // unknown is not "disabled": keep supervising and retry
	}
	if snap.ID == "" || !snap.Enabled {
		if p != nil && p.host != nil {
			s.stopHost(name, p, "disabled")
		}
		return false
	}
	if p = s.supervisedFor(name, p, snap.ID); p == nil {
		return true // its run state is refused: fails closed for this instance only
	}
	s.refresh(name, p, bindingKey{snap.ID, snap.Epoch, snap.Revision})
	return true
}

// supervisedFor returns the supervised state for the current installation,
// loading its run state when the installation is new to this controller.
func (s *supervisor) supervisedFor(name string, p *supervised, installation string) *supervised {
	if p != nil && p.run.Installation == installation {
		return p
	}
	if p != nil && p.host != nil {
		// A replaced installation: retire its host (stop and reap) first,
		// without writing its metadata over the new installation's file.
		p.host.stopped = true
		p.host.stop()
		p.host = nil
		s.logf("%s: host of a replaced installation stopped", name)
	}
	run, err := loadRunState(s.state, name, installation)
	if err != nil {
		s.logf("%s: run state refused: %s", name, model.ErrorCodeOf(err))
		return nil
	}
	p = &supervised{run: run}
	if run.State == runStarting || run.State == runRunning {
		// Left by an earlier controller: never trusted, never counted, and
		// no second host until its end is confirmed or the grace passes.
		p.recoverUntil = s.now().Add(recoveryGrace)
	}
	s.plugins[name] = p
	return p
}

// recoveryGrace bounds the wait for an earlier controller's host to end
// after its lifeline closed (the host's own teardown bound plus margin).
const recoveryGrace = lifelineGrace + 2*time.Second

// recovered reconciles a run left by an earlier controller through the
// plugin lease: free means that run ended (not counted); still held after
// the grace means the owner must check it (no automatic start).
func (s *supervisor) recovered(name string, p *supervised) bool {
	if p.recoverUntil.IsZero() {
		return true
	}
	lease, err := pluginfeed.OpenPluginLease(filepath.Join(s.state, "plugins", name+".lock"))
	if err == nil {
		_ = lease.Close()
	}
	// A running host held the lease, so a free one means it ended. A
	// starting host may not have taken it yet (it reads its record first):
	// only the grace, longer than its lifeline teardown, proves it ended.
	ended := err == nil && (p.run.State == runRunning || !s.now().Before(p.recoverUntil))
	switch {
	case ended:
		p.recoverUntil = time.Time{}
		p.run.recordStopped(s.now(), "prior_run_ended")
		s.save(name, p)
		return true
	case s.now().Before(p.recoverUntil):
		return false
	}
	p.recoverUntil = time.Time{}
	p.run.recordPermanent(s.now(), "prior_run_unconfirmed")
	s.logf("%s: an earlier run is not confirmed ended; check it, then run zalki plugins restart %s", name, name)
	s.save(name, p)
	return false
}

// refresh applies a restart request, replaces a host whose binding or log
// ended, and starts the instance when it is due.
func (s *supervisor) refresh(name string, p *supervised, key bindingKey) {
	if s.consumeRestart(name, p, key) && p.host != nil {
		s.stopHost(name, p, "restart_requested")
	}
	if p.host != nil && (p.binding != key || p.host.logs.failure() != nil) {
		reason := "binding_changed"
		if p.host.logs.failure() != nil {
			reason = "log_failed"
		}
		s.stopHost(name, p, reason)
	}
	if p.host == nil && s.recovered(name, p) && s.due(p) && s.durable(name, p) {
		s.start(name, p, key)
	}
	if p.noteHealthy(s.now()) {
		s.save(name, p)
	}
}

// due is startDue on the live monotonic retry anchor. A deadline read from
// disk (recovery) is clamped to the longest backoff, so a wall-clock jump
// can neither skip a wait nor stretch it past one step.
func (s *supervisor) due(p *supervised) bool {
	now := s.now()
	if p.run.State != runBackoff {
		return p.run.startDue(now)
	}
	if p.retryAt.IsZero() {
		wait := backoffSteps[len(backoffSteps)-1]
		if p.run.NextRetryAt != nil {
			wait = min(max(0, p.run.NextRetryAt.Sub(now)), wait)
		}
		p.retryAt = now.Add(wait)
	}
	return !now.Before(p.retryAt)
}

// fail counts one failure and anchors its retry on the monotonic clock.
func (s *supervisor) fail(p *supervised, now time.Time, reason string) {
	p.run.recordFailure(now, reason)
	p.retryAt = time.Time{}
	if p.run.NextRetryAt != nil {
		p.retryAt = now.Add(p.run.NextRetryAt.Sub(now))
	}
}

// noteHealthy resets the backoff after ten continuously observed healthy
// minutes on the live monotonic anchor; a handshake alone does not.
func (p *supervised) noteHealthy(now time.Time) bool {
	if p.run.State != runRunning || p.since.IsZero() || p.run.BackoffStep == 0 || now.Sub(p.since) < healthyReset {
		return false
	}
	p.run.BackoffStep = 0
	return true
}

func (s *supervisor) start(name string, p *supervised, key bindingKey) {
	now := s.now()
	logs, err := openRotatingLog(filepath.Join(s.state, "plugins", name+".log"))
	if err != nil {
		p.run.recordStopped(now, "log_failed")
		s.save(name, p)
		return
	}
	host, err := startHost(s.binary, s.state, s.home, name, logs, s.events)
	if err != nil {
		_ = logs.close()
		s.fail(p, now, "start_failed")
		s.save(name, p)
		return
	}
	p.host, p.binding, p.lastReason, p.retryAt, p.since = host, key, "", time.Time{}, time.Time{}
	p.run.Installation, p.run.Epoch, p.run.Revision = key.id, key.epoch, key.revision
	p.run.ControllerInstance, p.run.State, p.run.PID, p.run.PluginPID, p.run.NextRetryAt = s.instance, runStarting, host.cmd.Process.Pid, 0, nil
	s.save(name, p)
}

// save publishes the in-memory state. A failure (also one after the
// rename, when the result is uncertain) marks the instance unsaved; the
// in-memory state is kept as it is, never rolled back.
func (s *supervisor) save(name string, p *supervised) {
	if err := s.saveRun(s.state, name, p.run); err != nil {
		p.unsaved = true
		s.logf("%s: run state not saved (%s); starts are held until it is saved", name, model.ErrorCodeOf(err))
		return
	}
	p.unsaved = false
}

// durable retries an unsaved state and reports whether it is now saved.
func (s *supervisor) durable(name string, p *supervised) bool {
	if p.unsaved {
		s.save(name, p)
	}
	return !p.unsaved
}

// handle applies one host event. Events from a host we already replaced
// or stopped are stale and ignored.
func (s *supervisor) handle(ev hostEvent) {
	p := s.plugins[ev.name]
	if p == nil || p.host != ev.host {
		return
	}
	if ev.frame == nil {
		s.finish(ev.name, p)
		return
	}
	switch {
	case ev.frame.Phase == "ready" && ev.frame.Installation == p.binding.id && ev.frame.Epoch == p.binding.epoch:
		p.since = s.now()
		now := p.since.UTC()
		p.run.State, p.run.Since, p.run.PluginPID = runRunning, &now, ev.frame.PluginPID
		if p.run.FirstStartedAt == nil {
			p.run.FirstStartedAt = &now
		}
		s.save(ev.name, p)
	case ev.frame.Phase == "stopped" && validReason(ev.frame.Reason):
		p.lastReason = ev.frame.Reason
	}
}

// finish cleans up a host that ended on its own and classifies the end.
func (s *supervisor) finish(name string, p *supervised) {
	status := p.host.stop()
	p.host = nil
	now := s.now()
	reason := p.lastReason
	if changed := s.bindingEnded(name, p.binding); changed != "" {
		// The host saw its binding end (disable, revoke, scope or access
		// change) before our poll did: a stop, never a failure.
		p.run.recordStopped(now, changed)
		s.logf("%s: host ended: %s", name, changed)
		s.save(name, p)
		return
	}
	switch {
	case reason == "lease_held":
		p.run.recordStopped(now, reason)
	case permanentReasons[reason]:
		p.run.recordPermanent(now, reason)
	default:
		if reason == "" {
			reason = "failed"
			if status != nil && !status.Exited() {
				reason = "killed"
			}
		}
		// An exit while still enabled is unexpected, even with status 0.
		s.fail(p, now, reason)
	}
	s.logf("%s: host ended: %s", name, reason)
	s.save(name, p)
}

// bindingEnded reports why the binding a host ran for no longer holds, or
// "" while it is still the current, enabled one.
func (s *supervisor) bindingEnded(name string, ran bindingKey) string {
	snap, err := pluginfeed.NewPluginFeed(bus.Store{Dir: s.state}, name).PluginSnapshot()
	switch {
	case err != nil:
		return ""
	case snap.ID == "" || !snap.Enabled:
		return "disabled"
	case (bindingKey{snap.ID, snap.Epoch, snap.Revision}) != ran:
		return "binding_changed"
	}
	return ""
}

// stopHost is a requested stop: never counted as a failure.
func (s *supervisor) stopHost(name string, p *supervised, reason string) {
	// The intent is durable before the teardown: a controller that dies
	// mid-stop leaves "stopping", never a crash to count.
	p.run.Stopping = reason
	s.save(name, p)
	p.host.stopped = true
	p.host.stop()
	p.host = nil
	p.run.recordStopped(s.now(), reason)
	s.logf("%s: stopped: %s", name, reason)
	s.save(name, p)
}

// stopAll stops every host at once, so N plugins do not take N times the
// per-group bound.
func (s *supervisor) stopAll(reason string) {
	var wg sync.WaitGroup
	for name, p := range s.plugins {
		if p.host != nil {
			p.run.Stopping = reason
			s.save(name, p)
			wg.Add(1)
			go func(h *hostProcess) {
				defer wg.Done()
				h.stopped = true
				h.stop()
			}(p.host)
		}
	}
	wg.Wait()
	for name, p := range s.plugins {
		if p.host != nil {
			p.host = nil
			p.run.recordStopped(s.now(), reason)
			s.save(name, p)
		}
	}
}
