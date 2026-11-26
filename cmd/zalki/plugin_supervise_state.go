package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
)

// Lifecycle states persisted in NAME.run.json.
const (
	runStopped  = "stopped"
	runStarting = "starting"
	runRunning  = "running"
	runBackoff  = "backoff"
	runCrashed  = "crashed"
)

const (
	runStateLimit = 8 << 10
	crashLimit    = 5
	crashWindow   = 10 * time.Minute
	healthyReset  = 10 * time.Minute
)

// backoffSteps are the waits after failures 1, 2, 3, ...; the last one repeats.
var backoffSteps = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 60 * time.Second}

// pluginRunState is the controller's private runtime record for one
// instance. The controller is its only writer. It holds no capability,
// grant, message text or child error text: only controlled reason codes.
type pluginRunState struct {
	Schema             int    `json:"schema"`
	Installation       string `json:"installation"`
	Epoch              string `json:"epoch,omitempty"`
	Revision           string `json:"revision,omitempty"`
	ControllerInstance string `json:"controller_instance,omitempty"`
	State              string `json:"state"`
	PID                int    `json:"pid,omitempty"`
	PluginPID          int    `json:"plugin_pid,omitempty"`
	// HostStart is the OS start fingerprint of the host at PID (a bounded
	// string of digits, hex, '.', ':' and '-'), written in the same atomic
	// save as PID and cleared with it. Without it, a host is never shown as
	// running: a reused PID has another start.
	HostStart      string      `json:"host_start,omitempty"`
	Since          *time.Time  `json:"since,omitempty"`
	Restarts       []time.Time `json:"restarts,omitempty"`
	BackoffStep    int         `json:"backoff_step,omitempty"`
	NextRetryAt    *time.Time  `json:"next_retry_at,omitempty"`
	ReasonCode     string      `json:"reason_code,omitempty"`
	LastReasonAt   *time.Time  `json:"last_reason_at,omitempty"`
	FirstStartedAt *time.Time  `json:"first_started_at,omitempty"`
	// RestartSeq is the highest consumed restart request; every later
	// write keeps it, so an old request can never reset newer failures.
	RestartSeq uint64 `json:"restart_seq,omitempty"`
	// Stopping is the reason of an intentional stop, saved before the
	// teardown and cleared when it is recorded.
	Stopping string `json:"stopping,omitempty"`
	// Notified marks the first-start banner intent of this installation (an
	// intent, not proof of delivery), saved with the first start.
	Notified           bool   `json:"notified,omitempty"`
	NotificationState  string `json:"notification_state,omitempty"`
	NotificationReason string `json:"notification_reason,omitempty"`
}

func runStatePath(dir, name string) string {
	return filepath.Join(dir, "plugins", name+".run.json")
}

// loadRunState reads NAME.run.json strictly. A missing file is a fresh
// stopped state. Anything unsafe or malformed is an error that keeps the
// file as it is: a corrupt state never becomes five fresh attempts.
func loadRunState(dir, name, installation string) (pluginRunState, error) {
	path := runStatePath(dir, name)
	fresh := pluginRunState{Schema: 1, Installation: installation, State: runStopped}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fresh, nil
	}
	if err != nil {
		return fresh, err
	}
	if !privateFile(info) {
		return fresh, model.NewError(model.CodeConflict, "%s is not a private file of this user; it was kept", path)
	}
	raw, err := statefile.ReadBytes(path, runStateLimit)
	var state pluginRunState
	if err == nil {
		err = statefile.DecodeStrictRecordJSON(raw, &state)
	}
	if err != nil || !state.valid() || !canonicalJSON(raw, state) {
		return fresh, model.NewError(model.CodeConflict, "%s is damaged; it was kept", path)
	}
	if state.Installation != installation {
		// Another installation's history never carries over to this one.
		return fresh, nil
	}
	return state, nil
}

func privateFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0 && stat.Uid == uint32(os.Getuid()) //nolint:gosec // G115: getuid returns a Unix uid_t.
}

// canonicalJSON accepts only the exact bytes this controller writes: a
// duplicate key, a null, another order or a hand edit is not one of them.
func canonicalJSON(raw []byte, value any) bool {
	canonical, err := json.Marshal(value)
	return err == nil && bytes.Equal(canonical, raw)
}

// valid checks each field and the combinations each state requires; an
// impossible state is damaged, never a reason to start.
func (r pluginRunState) valid() bool {
	return r.validFields() && r.validForState()
}

func (r pluginRunState) validFields() bool {
	return r.validIdentity() && r.validCounters() && r.validStopping() && validTimes(r) && r.validNotification()
}

// validNotification: no state without the intent; a failure has its reason;
// a paused intent stays queued with the pause reason.
func (r pluginRunState) validNotification() bool {
	switch r.NotificationState {
	case "":
		return !r.Notified && r.NotificationReason == ""
	case notifyQueued:
		return r.Notified && (r.NotificationReason == "" || r.NotificationReason == notifyReasonUnknown || r.NotificationReason == notifyReasonBadState)
	case notifyDispatched:
		return r.Notified && r.NotificationReason == ""
	case notifyFailed:
		return r.Notified && r.NotificationReason == notifyReasonFailed
	}
	return false
}

func (r pluginRunState) validIdentity() bool {
	return r.Schema == 1 && model.IsCanonicalUUID(r.Installation) && (r.ControllerInstance == "" || model.IsCanonicalUUID(r.ControllerInstance))
}

func (r pluginRunState) validCounters() bool {
	return r.PID >= 0 && r.PluginPID >= 0 && r.BackoffStep >= 0 && r.BackoffStep <= len(backoffSteps) &&
		len(r.Restarts) <= crashLimit && validReason(r.ReasonCode) && validHostStart(r.HostStart, r.PID)
}

// validHostStart: empty, or a bounded fingerprint that exists only with a PID.
func validHostStart(start string, pid int) bool {
	if start == "" {
		return true
	}
	return pid > 0 && len(start) <= 64 && !strings.ContainsFunc(start, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f') && r != '.' && r != ':' && r != '-'
	})
}

// processAbsent reports positive evidence that no process has this PID.
func processAbsent(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrNotExist)
}

// validStopping: a stop intent names a known reason and exists only while
// a host is starting or running.
func (r pluginRunState) validStopping() bool {
	return r.Stopping == "" || (validReason(r.Stopping) && (r.State == runStarting || r.State == runRunning))
}

// validForState checks the combination each lifecycle state requires.
func (r pluginRunState) validForState() bool {
	switch r.State {
	case runStopped:
		return r.PID == 0 && r.NextRetryAt == nil
	case runStarting, runRunning:
		// Only a controller writes an active state, always with its ID.
		return r.PID > 0 && r.NextRetryAt == nil && (r.State == runStarting || r.Since != nil) && model.IsCanonicalUUID(r.ControllerInstance)
	case runBackoff:
		return r.validBackoff()
	case runCrashed:
		return r.PID == 0 && r.NextRetryAt == nil && r.ReasonCode != ""
	}
	return false
}

// validBackoff: a waiting state has its deadline, a grown step, the reason
// of its last end and below the crash limit; at least one counted failure
// unless it waits for a cause outside the plugin.
func (r pluginRunState) validBackoff() bool {
	counted := len(r.Restarts) > 0 || waitingReasons[r.ReasonCode]
	return r.PID == 0 && r.NextRetryAt != nil && counted && len(r.Restarts) < crashLimit && r.BackoffStep > 0 && r.ReasonCode != ""
}

func validTimes(r pluginRunState) bool {
	for _, at := range []*time.Time{r.Since, r.NextRetryAt, r.LastReasonAt, r.FirstStartedAt} {
		if at != nil && at.IsZero() {
			return false
		}
	}
	for _, at := range r.Restarts {
		if at.IsZero() {
			return false
		}
	}
	return true
}

// saveRunState publishes the state atomically and durably (mode 0600).
func saveRunState(dir, name string, state pluginRunState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > runStateLimit {
		return errors.New("plugin run state exceeds its limit")
	}
	return fsutil.WriteAtomic(runStatePath(dir, name), raw)
}

// recentFailures keeps the failures inside the rolling window, including
// one exactly at its start, and any future entry (after a clock rollback):
// a rollback never frees new attempts. At most crashLimit are kept.
func recentFailures(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-crashWindow)
	kept := make([]time.Time, 0, len(times))
	for _, at := range times {
		if !at.Before(cutoff) {
			kept = append(kept, at.UTC())
		}
	}
	return kept[max(0, len(kept)-crashLimit):]
}

// recordFailure counts one unexpected failure. The fifth inside the window
// is a crash with no further automatic start; otherwise the next start
// waits the current backoff step, which grows apart from the window count.
func (r *pluginRunState) recordFailure(now time.Time, reason string) {
	r.Restarts = recentFailures(append(r.Restarts, now), now)
	r.setReason(now, reason)
	r.PID, r.PluginPID, r.HostStart, r.Since = 0, 0, "", nil
	if len(r.Restarts) >= crashLimit {
		r.State, r.NextRetryAt = runCrashed, nil
		return
	}
	next := now.Add(backoffSteps[min(r.BackoffStep, len(backoffSteps)-1)]).UTC()
	r.BackoffStep = min(r.BackoffStep+1, len(backoffSteps))
	r.State, r.NextRetryAt = runBackoff, &next
}

// waitingReasons are causes outside the plugin (a held lease, a log that
// cannot be written): the start waits with the normal backoff steps, and
// nothing is counted as a crash.
var waitingReasons = map[string]bool{"lease_held": true, "log_failed": true}

// recordWaiting delays the next start by the next backoff step without a
// failure in the crash window.
func (r *pluginRunState) recordWaiting(now time.Time, reason string) {
	r.setReason(now, reason)
	r.PID, r.PluginPID, r.HostStart, r.Since = 0, 0, "", nil
	next := now.Add(backoffSteps[min(r.BackoffStep, len(backoffSteps)-1)]).UTC()
	r.BackoffStep = min(r.BackoffStep+1, len(backoffSteps))
	r.State, r.NextRetryAt = runBackoff, &next
}

// recordPermanent stops automatic starts: the owner must repair or
// reinstall. It is not counted as a crash in the window.
func (r *pluginRunState) recordPermanent(now time.Time, reason string) {
	r.setReason(now, reason)
	r.PID, r.PluginPID, r.HostStart, r.Since, r.NextRetryAt = 0, 0, "", nil, nil
	r.State = runCrashed
}

// recordStopped ends a run that was not a plugin failure (a stop, a
// binding change, a held lease): no count, no backoff change.
func (r *pluginRunState) recordStopped(now time.Time, reason string) {
	r.setReason(now, reason)
	r.PID, r.PluginPID, r.HostStart, r.Since, r.NextRetryAt = 0, 0, "", nil, nil
	r.State = runStopped
}

// setReason records an end; every end clears a stop intent.
func (r *pluginRunState) setReason(now time.Time, reason string) {
	at := now.UTC()
	r.ReasonCode, r.LastReasonAt, r.Stopping = reason, &at, ""
}

// startDue reports whether an automatic start may happen now.
func (r pluginRunState) startDue(now time.Time) bool {
	switch r.State {
	case runCrashed:
		return false
	case runBackoff:
		return r.NextRetryAt == nil || !now.Before(*r.NextRetryAt)
	}
	return true
}

// notifyFileName is the notification record inside the plugins folder.
const notifyFileName = ".supervisor-notifications.json"

// openPluginsDir opens state/plugins once, without following a link, and
// checks it is a private directory of this user. Every notification read and
// write is relative to this handle, so a replaced parent path is never used.
func openPluginsDir(state string) (*os.File, error) {
	fd, err := unix.Open(filepath.Join(state, "plugins"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), "plugins")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o022 != 0 || int(stat.Uid) != os.Getuid() {
		_ = dir.Close()
		return nil, errors.New("the plugins folder is not a private directory of this user")
	}
	return dir, nil
}

// readNotifyAt reads the record in dir: nil bytes when it does not exist.
// A link or a file that is not private is an error.
func readNotifyAt(dir *os.File) ([]byte, error) {
	// Non-blocking, so a FIFO or a device there cannot hold the open; the
	// type is then checked on the opened descriptor before any read.
	fd, err := unix.Openat(int(dir.Fd()), notifyFileName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), notifyFileName)
	defer file.Close() //nolint:errcheck // Read only.
	info, err := file.Stat()
	if err != nil || !privateFile(info) {
		return nil, errors.New("the notification state file is not a private file of this user")
	}
	return io.ReadAll(io.LimitReader(file, notifyFileLimit+1))
}

// writeNotifyAt replaces the record in dir atomically: a synced private
// temporary file, a rename inside dir and a synced dir. An error after the
// rename leaves the result unknown.
func writeNotifyAt(dir *os.File, raw []byte) error {
	tmp := notifyFileName + ".tmp-" + model.NewUUID()
	fd, err := unix.Openat(int(dir.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), tmp)
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = unix.Renameat(int(dir.Fd()), tmp, int(dir.Fd()), notifyFileName)
	}
	if err != nil {
		_ = unix.Unlinkat(int(dir.Fd()), tmp, 0)
		return err
	}
	return dir.Sync()
}
