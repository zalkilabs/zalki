package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// fakeHost is a host stand-in that runs until its lifeline (stdin) closes;
// it ignores its arguments and writes no frames.
func fakeHost(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-host")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec /bin/cat >/dev/null\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func startedFixture(t *testing.T) (*supervisor, *supervised) {
	t.Helper()
	s, p, key := restartFixture(t)
	s.binary, s.home, s.events, s.plugins = fakeHost(t), t.TempDir(), make(chan hostEvent, 8), map[string]*supervised{}
	p.run = pluginRunState{Schema: 1, Installation: key.id, State: runStopped}
	s.start("demo", p, key)
	if p.host == nil {
		t.Fatal("the fake host did not start")
	}
	s.plugins["demo"] = p
	return s, p
}

// A replaced installation's host is stopped and reaped before the new one
// is supervised, and the old metadata is not written over the new state.
func TestPluginSupervisorReplacedInstallationRetiresOldHost(t *testing.T) {
	s, p := startedFixture(t)
	pid := p.host.cmd.Process.Pid
	next := s.supervisedFor("demo", p, model.NewUUID())
	if p.host != nil || !hostProcessGone(pid) {
		t.Fatal("a replaced installation's host was left running")
	}
	if next == nil || next == p || next.host != nil || len(next.run.Restarts) != 0 {
		t.Fatal("the new installation inherited the old host or history")
	}
}

func writeRun(t *testing.T, s *supervisor, run pluginRunState) {
	t.Helper()
	if err := saveRunState(s.state, "demo", run); err != nil {
		t.Fatal(err)
	}
}

func priorRun(key bindingKey) pluginRunState {
	since := stateEpoch
	return pluginRunState{Schema: 1, Installation: key.id, ControllerInstance: model.NewUUID(), State: runRunning, PID: 4242, Since: &since,
		Restarts: []time.Time{stateEpoch.Add(-time.Minute)}}
}

// A run left by an earlier controller is reconciled through the plugin
// lease: free means it ended (not counted, history kept); still held after
// the grace means no automatic start until the owner restarts it.
func TestPluginSupervisorPriorRunReconciliation(t *testing.T) {
	s, _, key := restartFixture(t)
	s.plugins = map[string]*supervised{}
	writeRun(t, s, priorRun(key))
	p := s.supervisedFor("demo", nil, key.id)
	if p.recoverUntil.IsZero() || !s.recovered("demo", p) || p.run.State != runStopped || p.run.ReasonCode != "prior_run_ended" || len(p.run.Restarts) != 1 {
		t.Fatalf("an earlier run was not reconciled: %s/%s, %d failures", p.run.State, p.run.ReasonCode, len(p.run.Restarts))
	}
	lease, err := pluginfeed.OpenPluginLease(filepath.Join(s.state, "plugins", "demo.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	writeRun(t, s, priorRun(key))
	now := stateEpoch
	s.now = func() time.Time { return now }
	p = s.supervisedFor("demo", nil, key.id)
	if s.recovered("demo", p) || p.run.State != runRunning {
		t.Fatal("a second host could start while the earlier run still held its lease")
	}
	now = now.Add(recoveryGrace)
	if s.recovered("demo", p) || p.run.State != runCrashed || p.run.ReasonCode != "prior_run_unconfirmed" || p.run.startDue(now.Add(time.Hour)) {
		t.Fatalf("an unconfirmed earlier run can start again: %s/%s", p.run.State, p.run.ReasonCode)
	}
}

// A prior run that was still starting may not have taken its lease yet:
// a free lease is not proof it ended, so it waits for the grace.
func TestPluginSupervisorPriorStartingRunWaitsForGrace(t *testing.T) {
	s, _, key := restartFixture(t)
	s.plugins = map[string]*supervised{}
	now := stateEpoch
	s.now = func() time.Time { return now }
	starting := priorRun(key)
	starting.State, starting.Since = runStarting, nil
	writeRun(t, s, starting)
	p := s.supervisedFor("demo", nil, key.id)
	if s.recovered("demo", p) || p.run.State != runStarting {
		t.Fatal("a prior starting run was treated as ended before its grace")
	}
	now = now.Add(recoveryGrace)
	if !s.recovered("demo", p) || p.run.State != runStopped || p.run.ReasonCode != "prior_run_ended" {
		t.Fatalf("a prior starting run did not end after its grace: %s/%s", p.run.State, p.run.ReasonCode)
	}
}

// An intentional stop is durable before the teardown, and cleared after.
func TestPluginSupervisorStopIntentIsDurableFirst(t *testing.T) {
	s, p := startedFixture(t)
	var saved []pluginRunState
	s.saveRun = func(dir, name string, run pluginRunState) error {
		saved = append(saved, run)
		return saveRunState(dir, name, run)
	}
	s.stopHost("demo", p, "disabled")
	if len(saved) < 2 || saved[0].Stopping != "disabled" || saved[0].State != runStarting {
		t.Fatal("the stop intent was not durable before the teardown")
	}
	if last := saved[len(saved)-1]; last.Stopping != "" || last.State != runStopped || last.ReasonCode != "disabled" {
		t.Fatalf("the stop was recorded as %s/%s, intent %q", last.State, last.ReasonCode, last.Stopping)
	}
}

// The healthy timer and retry waits use the live monotonic clock; a
// deadline read from disk is clamped to the longest backoff.
func TestPluginSupervisorMonotonicAnchors(t *testing.T) {
	anchor := time.Now()
	wall := anchor.Add(-11 * time.Minute).UTC()
	healthy := &supervised{run: pluginRunState{Schema: 1, State: runRunning, BackoffStep: 2, Since: &wall}, since: anchor}
	if healthy.noteHealthy(anchor.Add(time.Minute)) {
		t.Fatal("a wall-clock jump reset the backoff")
	}
	now := anchor.Add(time.Second)
	s := &supervisor{now: func() time.Time { return now }}
	past := anchor.Add(-time.Hour).UTC()
	waiting := &supervised{run: pluginRunState{Schema: 1, State: runBackoff, NextRetryAt: &past}, retryAt: anchor.Add(4 * time.Second)}
	if s.due(waiting) {
		t.Fatal("a wall-clock jump skipped a retry wait")
	}
	far := anchor.Add(time.Hour).UTC()
	recovered := &supervised{run: pluginRunState{Schema: 1, State: runBackoff, NextRetryAt: &far}}
	if s.due(recovered) {
		t.Fatal("a recovered deadline started at once")
	}
	now = now.Add(backoffSteps[len(backoffSteps)-1] + time.Second)
	if !s.due(recovered) {
		t.Fatal("a recovered deadline was not clamped to the longest backoff")
	}
}

// Nothing is created through a plugins folder that is a link or that
// others can write.
func TestPluginSupervisorRefusesUnsafePluginsDir(t *testing.T) {
	for name, plant := range map[string]func(state, target string) error{
		"link":     func(state, target string) error { return os.Symlink(target, filepath.Join(state, "plugins")) },
		"writable": func(state, _ string) error { return os.Mkdir(filepath.Join(state, "plugins"), 0o777) },
	} {
		state, target := t.TempDir(), t.TempDir()
		if err := plant(state, target); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(filepath.Join(state, "plugins"), 0o777) // umask may narrow Mkdir
		err := runSupervisor(context.Background(), state, true)
		entries, _ := os.ReadDir(target)
		own, _ := os.ReadDir(filepath.Join(state, "plugins"))
		if err != errUnsafePluginsDir || commandExitCode(err) != 0 || len(entries) != 0 || (name == "writable" && len(own) != 0) { //nolint:errorlint // Direct identity is the contract.
			t.Fatalf("%s: files were written through an unsafe plugins directory: %v", name, err)
		}
	}
}

// An oversize active log is refused, and a replaced log file is kept, not
// rotated.
func TestPluginSupervisorLogIdentityAndSize(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), logFileLimit+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if logs, err := openRotatingLog(big); err == nil {
		_ = logs.close()
		t.Fatal("an oversize log was opened")
	}
	path := filepath.Join(dir, "demo.log")
	logs, err := openRotatingLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.close()
	err = os.WriteFile(path+".new", []byte("someone else"), 0o600)
	if err == nil {
		err = os.Rename(path+".new", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	err = logs.write(bytes.Repeat([]byte("x"), logFileLimit+1))
	if raw, _ := os.ReadFile(path); err == nil || string(raw) != "someone else" {
		t.Fatal("a replaced log was rotated")
	}
}

// A listing or executable check that fails for an unknown reason is an
// error to retry, never "no plugins" or "uninstalled" (exit 0).
func TestPluginSupervisorUnknownErrorsAreNotAbsence(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cuts need an ordinary user")
	}
	s, _, _ := restartFixture(t)
	s.plugins = map[string]*supervised{}
	dir := filepath.Join(s.state, "plugins")
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	active, err := s.scan()
	_ = os.Chmod(dir, 0o700)
	if err == nil || !active {
		t.Fatal("an unreadable plugins folder was read as no plugins")
	}
	s.binaryID = binaryIdentity{dev: 1}
	s.resolve = func(string) (binaryIdentity, error) { return binaryIdentity{}, syscall.EACCES }
	if s.checkBinary() != binaryUnknown {
		t.Fatal("an unreadable executable was read as missing")
	}
}

// S14: after lifeline EOF a host ends within its bound even when its run
// never returns (stuck, for example, on a held journal lock): the child
// below blocks forever and ignores cancellation.
func TestPluginSupervisedHostLifelineIsBounded(t *testing.T) {
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestPluginLifelineChild$")
	cmd.Env = append(os.Environ(), "ZALKI_LIFELINE_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	time.Sleep(500 * time.Millisecond) // the child is in its run
	_ = stdin.Close()
	select {
	case <-done:
	case <-time.After(lifelineGrace + 3*time.Second):
		// Only the direct child: a group signal could reach a reused PGID
		// once Wait has reaped the leader.
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("a stuck host outlived its lifeline bound")
	}
}

func TestPluginLifelineChild(t *testing.T) {
	if os.Getenv("ZALKI_LIFELINE_CHILD") != "1" {
		return
	}
	// A sleeping run (not a bare select, which the runtime would end as a
	// deadlock) that never returns and ignores cancellation.
	err := superviseLifeline(context.Background(), os.Stdin, func(context.Context) error {
		time.Sleep(time.Hour)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A restart reset whose save is uncertain (an error after the rename)
// keeps the new high-water mark in memory and holds starts, so a later
// ordinary save can never write the old mark back.
func TestPluginRestartUncertainSaveKeepsTheMark(t *testing.T) {
	s, p, key := restartFixture(t)
	writeRequest(t, s.state, requestFor(key, 1))
	s.saveRun = func(dir, name string, run pluginRunState) error {
		return errors.Join(saveRunState(dir, name, run), errors.New("directory sync failed"))
	}
	s.consumeRestart("demo", p, key)
	if p.run.RestartSeq != 1 || !p.unsaved {
		t.Fatalf("an uncertain restart save lost its mark: seq %d, unsaved %v", p.run.RestartSeq, p.unsaved)
	}
	s.saveRun = saveRunState
	s.save("demo", p)
	if saved, err := loadRunState(s.state, "demo", key.id); err != nil || saved.RestartSeq != 1 {
		t.Fatalf("a later save wrote the old mark back: %d (%v)", saved.RestartSeq, err)
	}
}

// Only a positively unsafe plugins folder exits 0; an unknown error while
// checking it exits 1 so a new supervisor retries.
func TestPluginSupervisorUnknownDirErrorIsNotPermanent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cuts need an ordinary user")
	}
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(state, 0o700)
	err := runSupervisor(context.Background(), state, true)
	if err == nil || commandExitCode(err) != 1 {
		t.Fatalf("an unknown directory error exited 0: %v", err)
	}
}

// M2: a run that completes before lifeline EOF is seen (the lifeline here
// never closes) still ends the host's whole group: the descendant it left
// does not outlive the host.
func TestPluginSupervisedHostCompletedRunEndsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestPluginCompletedRunChild$")
	cmd.Env = append(os.Environ(), "ZALKI_COMPLETED_RUN_CHILD="+pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe() // kept open: lifeline EOF never comes
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("the completed host did not exit")
	}
	raw, err := os.ReadFile(pidFile)
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || convErr != nil || pid <= 0 {
		t.Fatal("the child did not report its descendant", err, convErr)
	}
	// No signal to a PID read from a file: the fixture's descendant is
	// /bin/sleep 60 and ends on its own.
	if !awaitGone(pid) {
		t.Fatal("a descendant outlived a completed run")
	}
}

func TestPluginCompletedRunChild(t *testing.T) {
	pidFile := os.Getenv("ZALKI_COMPLETED_RUN_CHILD")
	if pidFile == "" {
		return
	}
	err := superviseLifeline(context.Background(), os.Stdin, func(context.Context) error {
		descendant := exec.CommandContext(context.Background(), "/bin/sleep", "60")
		if err := descendant.Start(); err != nil {
			return err
		}
		return os.WriteFile(pidFile, []byte(strconv.Itoa(descendant.Process.Pid)), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// M1: a reset whose save failed BEFORE publication keeps its request while
// the new mark is only in memory; replays do not delete it. Only after a
// save confirms the mark is the request cleaned up.
func TestPluginRestartPrePublicationFailureKeepsRequest(t *testing.T) {
	s, p, key := restartFixture(t)
	writeRequest(t, s.state, requestFor(key, 1))
	s.saveRun = func(string, string, pluginRunState) error { return errors.New("disk full") }
	s.consumeRestart("demo", p, key)
	s.consumeRestart("demo", p, key) // a replay while the mark is unsaved
	if _, err := os.Lstat(restartPath(s.state, "demo")); err != nil || !p.unsaved {
		t.Fatalf("a restart request was deleted before its mark was durable: %v", err)
	}
	s.saveRun = saveRunState
	s.save("demo", p)
	s.consumeRestart("demo", p, key)
	saved, err := loadRunState(s.state, "demo", key.id)
	if _, statErr := os.Lstat(restartPath(s.state, "demo")); !os.IsNotExist(statErr) || err != nil || saved.RestartSeq != 1 {
		t.Fatalf("after the confirmed save the request was not cleaned up or the mark is missing: %v %v %d", statErr, err, saved.RestartSeq)
	}
}
