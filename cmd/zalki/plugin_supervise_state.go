package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
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
	Schema             int         `json:"schema"`
	Installation       string      `json:"installation"`
	Epoch              string      `json:"epoch,omitempty"`
	Revision           string      `json:"revision,omitempty"`
	ControllerInstance string      `json:"controller_instance,omitempty"`
	State              string      `json:"state"`
	PID                int         `json:"pid,omitempty"`
	PluginPID          int         `json:"plugin_pid,omitempty"`
	Since              *time.Time  `json:"since,omitempty"`
	Restarts           []time.Time `json:"restarts,omitempty"`
	BackoffStep        int         `json:"backoff_step,omitempty"`
	NextRetryAt        *time.Time  `json:"next_retry_at,omitempty"`
	ReasonCode         string      `json:"reason_code,omitempty"`
	LastReasonAt       *time.Time  `json:"last_reason_at,omitempty"`
	FirstStartedAt     *time.Time  `json:"first_started_at,omitempty"`
	// RestartSeq is the highest consumed restart request; every later
	// write keeps it, so an old request can never reset newer failures.
	RestartSeq uint64 `json:"restart_seq,omitempty"`
	// Stopping is the reason of an intentional stop, saved before the
	// teardown and cleared when it is recorded.
	Stopping string `json:"stopping,omitempty"`
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
	raw, err := journal.ReadBytes(path, runStateLimit)
	var state pluginRunState
	if err == nil {
		err = bus.DecodeStrictRecordJSON(raw, &state)
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
	return r.validIdentity() && r.validCounters() && r.validStopping() && validTimes(r)
}

func (r pluginRunState) validIdentity() bool {
	return r.Schema == 1 && model.IsCanonicalUUID(r.Installation) && (r.ControllerInstance == "" || model.IsCanonicalUUID(r.ControllerInstance))
}

func (r pluginRunState) validCounters() bool {
	return r.PID >= 0 && r.PluginPID >= 0 && r.BackoffStep >= 0 && r.BackoffStep <= len(backoffSteps) &&
		len(r.Restarts) <= crashLimit && validReason(r.ReasonCode)
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

// validBackoff: a waiting state has its deadline, 1-4 counted failures, a
// grown step and the reason of its last failure.
func (r pluginRunState) validBackoff() bool {
	return r.PID == 0 && r.NextRetryAt != nil && len(r.Restarts) > 0 && len(r.Restarts) < crashLimit && r.BackoffStep > 0 && r.ReasonCode != ""
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
	return journal.WriteAtomic(runStatePath(dir, name), raw)
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
	r.PID, r.PluginPID, r.Since = 0, 0, nil
	if len(r.Restarts) >= crashLimit {
		r.State, r.NextRetryAt = runCrashed, nil
		return
	}
	next := now.Add(backoffSteps[min(r.BackoffStep, len(backoffSteps)-1)]).UTC()
	r.BackoffStep = min(r.BackoffStep+1, len(backoffSteps))
	r.State, r.NextRetryAt = runBackoff, &next
}

// recordPermanent stops automatic starts: the owner must repair or
// reinstall. It is not counted as a crash in the window.
func (r *pluginRunState) recordPermanent(now time.Time, reason string) {
	r.setReason(now, reason)
	r.PID, r.PluginPID, r.Since, r.NextRetryAt = 0, 0, nil, nil
	r.State = runCrashed
}

// recordStopped ends a run that was not a plugin failure (a stop, a
// binding change, a held lease): no count, no backoff change.
func (r *pluginRunState) recordStopped(now time.Time, reason string) {
	r.setReason(now, reason)
	r.PID, r.PluginPID, r.Since, r.NextRetryAt = 0, 0, nil, nil
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
