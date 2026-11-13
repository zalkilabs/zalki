package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

var stateEpoch = time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC)

// S11: failures spaced three minutes apart never reach five in ten minutes,
// yet the backoff still grows 1, 2, 4, 8, 16, 32, 60 and stays at 60.
func TestPluginCrashBackoffSteps(t *testing.T) {
	run := pluginRunState{Schema: 1, Installation: model.NewUUID(), State: runStopped}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	now := stateEpoch
	for i, wait := range want {
		run.recordFailure(now, "failed")
		if run.State != runBackoff || run.NextRetryAt == nil || run.NextRetryAt.Sub(now) != wait*time.Second {
			t.Fatalf("failure %d: state %s, wait %v, want %v", i+1, run.State, run.NextRetryAt.Sub(now), wait*time.Second)
		}
		if run.startDue(now.Add(wait*time.Second-time.Millisecond)) || !run.startDue(now.Add(wait*time.Second)) {
			t.Fatalf("failure %d: start allowed before its deadline or refused at it", i+1)
		}
		now = now.Add(3 * time.Minute)
	}
}

// S11: the fifth failure inside the window, counting one exactly at its
// start, is a crash with no further automatic start.
func TestPluginCrashWindowIsInclusive(t *testing.T) {
	run := pluginRunState{Schema: 1, Installation: model.NewUUID(), State: runStopped}
	run.recordFailure(stateEpoch, "failed")
	for i := 1; i < 4; i++ {
		run.recordFailure(stateEpoch.Add(time.Duration(i)*time.Minute), "failed")
	}
	run.recordFailure(stateEpoch.Add(crashWindow), "failed") // the first is exactly at now-10m
	if run.State != runCrashed || run.startDue(stateEpoch.Add(time.Hour)) {
		t.Fatalf("fifth failure in the inclusive window: state %s", run.State)
	}
	older := pluginRunState{Schema: 1, Installation: model.NewUUID(), State: runStopped}
	older.recordFailure(stateEpoch, "failed")
	for i := 1; i < 4; i++ {
		older.recordFailure(stateEpoch.Add(time.Duration(i)*time.Minute), "failed")
	}
	older.recordFailure(stateEpoch.Add(crashWindow+time.Nanosecond), "failed")
	if older.State != runBackoff || len(older.Restarts) != 4 {
		t.Fatalf("a failure just outside the window was counted: %s, %d", older.State, len(older.Restarts))
	}
}

// S11: after a clock rollback, future entries are kept, so a rollback never
// frees five new attempts.
func TestPluginCrashWindowClockRollback(t *testing.T) {
	run := pluginRunState{Schema: 1, Installation: model.NewUUID(), State: runStopped}
	for i := range 4 {
		run.recordFailure(stateEpoch.Add(time.Duration(i)*time.Minute), "failed")
	}
	run.recordFailure(stateEpoch.Add(-time.Hour), "failed")
	if run.State != runCrashed {
		t.Fatalf("a clock rollback discarded failures: %s, %d", run.State, len(run.Restarts))
	}
}

// S11: neither a handshake nor a short healthy run resets the backoff;
// ten continuous healthy minutes do.
func TestPluginBackoffResetsAfterTenHealthyMinutes(t *testing.T) {
	run := pluginRunState{Schema: 1, Installation: model.NewUUID(), State: runStopped}
	run.recordFailure(stateEpoch, "failed")
	run.recordFailure(stateEpoch.Add(time.Minute), "failed")
	since := stateEpoch.Add(2 * time.Minute)
	run.State, run.Since = runRunning, &since
	p := &supervised{run: run, since: since}
	if p.noteHealthy(since.Add(healthyReset-time.Second)) || p.run.BackoffStep != 2 {
		t.Fatal("the backoff was reset before ten healthy minutes")
	}
	if !p.noteHealthy(since.Add(healthyReset)) || p.run.BackoffStep != 0 {
		t.Fatal("ten healthy minutes did not reset the backoff")
	}
}

// Permanent reasons crash without counting; held leases and requested stops
// neither count nor back off.
func TestPluginStopKinds(t *testing.T) {
	run := pluginRunState{Schema: 1, Installation: model.NewUUID(), State: runRunning}
	run.recordStopped(stateEpoch, "lease_held")
	if run.State != runStopped || len(run.Restarts) != 0 || run.BackoffStep != 0 || !run.startDue(stateEpoch) {
		t.Fatalf("a held lease was charged: %+v", run)
	}
	run.recordPermanent(stateEpoch, "pin_changed")
	if run.State != runCrashed || len(run.Restarts) != 0 || run.startDue(stateEpoch.Add(time.Hour)) {
		t.Fatalf("a permanent reason retried or counted: %+v", run)
	}
}

// S09/S10: the state survives a save and load; an unsafe or damaged file is
// refused and kept byte for byte; another installation starts fresh.
func TestPluginRunStateRefusesUnsafeOrForeign(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := model.NewUUID()
	run := pluginRunState{Schema: 1, Installation: id, State: runStopped}
	for i := range 5 {
		run.recordFailure(stateEpoch.Add(time.Duration(i)*time.Minute), "failed")
	}
	if err := saveRunState(dir, "demo", run); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadRunState(dir, "demo", id)
	if err != nil || loaded.State != runCrashed || len(loaded.Restarts) != crashLimit {
		t.Fatalf("a crashed state did not survive a restart: %+v %v", loaded, err)
	}
	if other, err := loadRunState(dir, "demo", model.NewUUID()); err != nil || other.State != runStopped || len(other.Restarts) != 0 {
		t.Fatalf("another installation inherited the history: %+v %v", other, err)
	}
	checkUnsafeRunStates(t, dir, id, run)
}

// checkUnsafeRunStates plants each unsafe file over a saved state and
// checks it is refused and kept byte for byte.
func checkUnsafeRunStates(t *testing.T, dir, id string, run pluginRunState) {
	t.Helper()
	path := runStatePath(dir, "demo")
	for name, plant := range map[string]func() error{
		"partial": func() error { return os.WriteFile(path, []byte(`{"schema":1,`), 0o600) },
		"bad state": func() error {
			return os.WriteFile(path, []byte(`{"schema":1,"installation":"`+id+`","state":"dancing"}`), 0o600)
		},
		"other schema": func() error {
			return os.WriteFile(path, []byte(`{"schema":2,"installation":"`+id+`","state":"stopped"}`), 0o600)
		},
		"unknown key": func() error {
			return os.WriteFile(path, []byte(`{"schema":1,"installation":"`+id+`","state":"stopped","x":1}`), 0o600)
		},
		"readable": func() error { return os.Chmod(path, 0o644) },
		"duplicate key": func() error {
			return os.WriteFile(path, []byte(`{"schema":1,"installation":"`+id+`","state":"stopped","state":"stopped"}`), 0o600)
		},
		"null value": func() error {
			return os.WriteFile(path, []byte(`{"schema":1,"installation":"`+id+`","state":"stopped","since":null}`), 0o600)
		},
		"backoff without deadline": func() error {
			return saveRunState(dir, "demo", pluginRunState{Schema: 1, Installation: id, State: runBackoff, Restarts: []time.Time{stateEpoch}, BackoffStep: 1, ReasonCode: "failed"})
		},
		"running without controller": func() error {
			at := stateEpoch
			return saveRunState(dir, "demo", pluginRunState{Schema: 1, Installation: id, State: runRunning, PID: 4242, Since: &at})
		},
		"running without pid": func() error {
			at := stateEpoch
			return saveRunState(dir, "demo", pluginRunState{Schema: 1, Installation: id, State: runRunning, Since: &at})
		},
		"link": func() error {
			_ = os.Remove(path)
			return os.Symlink(filepath.Join(dir, "elsewhere"), path)
		},
	} {
		_ = os.Remove(path)
		if err := saveRunState(dir, "demo", run); err != nil {
			t.Fatal(err)
		}
		if err := plant(); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Readlink(path)
		raw, _ := os.ReadFile(path)
		if _, err := loadRunState(dir, "demo", id); err == nil || !strings.Contains(err.Error(), "it was kept") {
			t.Errorf("%s: an unsafe run state was accepted: %v", name, err)
		}
		after, _ := os.Readlink(path)
		rawAfter, _ := os.ReadFile(path)
		if before != after || string(raw) != string(rawAfter) {
			t.Errorf("%s: the refused file was changed", name)
		}
	}
}

// A counted failure and its deadline must be durable before any retry:
// after a failed or uncertain save the next start is held, the in-memory
// count is kept, and the start resumes once a save succeeds.
func TestPluginSupervisorFailedSaveHoldsStart(t *testing.T) {
	for name, failing := range map[string]func(dir, name string, run pluginRunState) error{
		"write failed": func(string, string, pluginRunState) error { return errors.New("disk full") },
		"uncertain after rename": func(dir, name string, run pluginRunState) error {
			return errors.Join(saveRunState(dir, name, run), errors.New("directory sync failed"))
		},
	} {
		t.Run(name, func(t *testing.T) { checkFailedSaveHoldsStart(t, failing) })
	}
}

func checkFailedSaveHoldsStart(t *testing.T, failing func(dir, name string, run pluginRunState) error) {
	t.Helper()
	s, p, key := restartFixture(t)
	s.binary, s.home = "/nonexistent/zalki", t.TempDir() // any start attempt fails and is counted
	now := stateEpoch
	s.now = func() time.Time { return now }
	p.run = pluginRunState{Schema: 1, Installation: key.id, State: runStopped}
	s.saveRun = failing
	p.run.recordFailure(now, "failed")
	s.save("demo", p)
	now = now.Add(time.Minute) // past the backoff deadline
	s.refresh("demo", p, key)
	if p.host != nil || len(p.run.Restarts) != 1 || p.run.State != runBackoff {
		t.Fatalf("a start ran before its counted failure was durable: %d failures, state %s", len(p.run.Restarts), p.run.State)
	}
	s.saveRun = saveRunState
	s.refresh("demo", p, key)
	saved, err := loadRunState(s.state, "demo", key.id)
	if p.unsaved || len(p.run.Restarts) != 2 || err != nil || len(saved.Restarts) != 2 {
		t.Fatalf("the start did not resume after a successful save: %d failures, saved %d (%v)", len(p.run.Restarts), len(saved.Restarts), err)
	}
}
