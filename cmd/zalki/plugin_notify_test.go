package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// notifyClock is a fake monotonic and wall clock for the banner budget.
type notifyClock struct {
	mono time.Duration
	wall time.Time
}

// testNotifier opens the plugins folder like notifierFor; a missing folder
// leaves it nil (every attempt pauses).
func testNotifier(state, controller string, c *notifyClock) *notifier {
	dir, _ := openPluginsDir(state)
	return &notifier{
		state: state, controller: controller, dir: dir, write: writeNotifyAt,
		budget:  notifyBudget{elapsed: func() time.Duration { return c.mono }, wall: func() time.Time { return c.wall }},
		queue:   make(chan notifyIntent, notifyQueueSize),
		results: make(chan notifyResult, 2*notifyQueueSize),
		run:     runNotifyChild,
		done:    make(chan struct{}),
	}
}

// notifyFake is the owned banner fixture. Each call records its argv;
// files in dir pick a mode: fail (exit 1), loud (5000 bytes of output),
// sleep (5 s, then a slept marker), quiet (closes its output, then exits 0
// after 1 s), leave (moves itself into the test's group, then sleeps 8 s).
type notifyFake struct{ dir string }

const notifyFixtureScript = `#!/bin/sh
[ "$1" = "--warm" ] && exit 0
d=%q
/usr/bin/printf '%%s\0' "$@" > "$d/call.$$"
[ -f "$d/fail" ] && exit 1
[ -f "$d/loud" ] && /usr/bin/head -c 5000 /dev/zero
[ -f "$d/sleep" ] && { /bin/sleep 5; /usr/bin/touch "$d/slept.$$"; }
[ -f "$d/quiet" ] && { exec >&- 2>&-; /bin/sleep 1; exit 0; }
[ -f "$d/leave" ] && exec /usr/bin/perl -e 'setpgrp(0, getpgrp(getppid())) or exit 9; open(my $f, ">", $ARGV[0]); close($f); sleep 8' "$d/left"
exit 0
`

func useNotifyFixture(t *testing.T) notifyFake {
	t.Helper()
	fake := notifyFake{dir: t.TempDir()}
	writeNotifyFixture(t, fake.dir)
	old := notifyExecutable
	notifyExecutable = filepath.Join(fake.dir, "notifier")
	t.Cleanup(func() { notifyExecutable = old })
	return fake
}

func writeNotifyFixture(t *testing.T, dir string) {
	t.Helper()
	script := fmt.Sprintf(notifyFixtureScript, dir)
	path := filepath.Join(dir, "notifier")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// Run it once before any test clock starts: the first exec of a new
	// script can be slow on macOS, and must not race the 2 s watchdog.
	warm := exec.CommandContext(t.Context(), path, "--warm")
	warm.Env = []string{"PATH=/usr/bin:/bin"}
	if err := warm.Run(); err != nil {
		t.Fatal("the fixture did not run:", err)
	}
}

func (f notifyFake) mode(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// calls are the recorded argv lists, one per child.
func (f notifyFake) calls(t *testing.T) [][]string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(f.dir, "call.*"))
	calls := make([][]string, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"))
	}
	return calls
}

// notifySupervisor is a supervisor for one manual plugin "demo" with a
// fake host and a fixture notifier on a fake clock past the hold.
func notifySupervisor(t *testing.T) (*supervisor, notifyFake, *notifyClock) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	if raw, err := pluginOwnerLocal(t, store.Dir, "add", "demo", "--exec", feedFixture(t)); err != nil {
		t.Fatal(string(raw), err)
	}
	s := newNotifySupervisor(t, store.Dir)
	fake := useNotifyFixture(t)
	clock := &notifyClock{mono: notifyWindow, wall: stateEpoch}
	s.notify = testNotifier(store.Dir, s.instance, clock)
	return s, fake, clock
}

// newNotifySupervisor is a fresh controller generation for state.
func newNotifySupervisor(t *testing.T, state string) *supervisor {
	t.Helper()
	logs, err := openRotatingLog(filepath.Join(state, "plugins", ".supervisor.log"))
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{state: state, log: logs, saveRun: saveRunState, instance: model.NewUUID(), now: time.Now, binary: fakeHost(t), home: t.TempDir(),
		events: make(chan hostEvent, 64), plugins: map[string]*supervised{}, offered: map[string]bool{}}
	t.Cleanup(func() {
		s.stopAll("supervisor_stopped")
		_ = logs.close()
	})
	return s
}

// startDemo starts "demo" the way reconcile does for a due instance.
func startDemo(t *testing.T, s *supervisor) *supervised {
	t.Helper()
	id := installationID(t, s.state, "demo")
	p := s.supervisedFor("demo", s.plugins["demo"], id)
	if p == nil {
		t.Fatal("the run state was refused")
		return nil
	}
	s.start("demo", p, bindingKey{id: id})
	if p.host == nil {
		t.Fatal("the host did not start")
	}
	return p
}

// drain runs every offered intent through the worker step and applies the
// results, as the controller loop does.
func drain(t *testing.T, s *supervisor, clock *notifyClock) {
	t.Helper()
	for {
		select {
		case it := <-s.notify.queue:
			wait, res := s.notify.try(it)
			if wait > 0 {
				clock.mono += wait
				wait, res = s.notify.try(it)
			}
			if wait > 0 {
				t.Fatalf("the budget still waits %s", wait)
			}
			if res != nil {
				s.applyNotify(*res)
			}
		default:
			return
		}
	}
}

func savedRun(t *testing.T, s *supervisor) pluginRunState {
	t.Helper()
	run, err := loadRunState(s.state, "demo", installationID(t, s.state, "demo"))
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// S18: the banner copies slice4's receipt evidence; a plugin without proof
// or without a receipt gets the exact unverified-installer sentence, and
// status keeps "unknown" for it.
func TestPluginBackgroundInstalledBy(t *testing.T) {
	session := &pluginfeed.PluginInstallReceipt{InstalledBy: "00000000-0000-4000-8000-0000000000aa", InstalledByLabel: "build bot"}
	for _, tc := range []struct {
		name    string
		receipt *pluginfeed.PluginInstallReceipt
		want    string
	}{
		{"session label", session, `new background plugin "demo", installed by session "build bot"`},
		{"session id", &pluginfeed.PluginInstallReceipt{InstalledBy: session.InstalledBy}, `new background plugin "demo", installed by session "` + session.InstalledBy + `"`},
		{"terminal", &pluginfeed.PluginInstallReceipt{InstalledBy: "terminal"}, `new background plugin "demo", installed by terminal`},
		{"unproven", &pluginfeed.PluginInstallReceipt{InstalledBy: "unproven", InstalledByLabel: "ignored"}, `new background plugin "demo", installed by an unverified installer`},
		{"no receipt", nil, `new background plugin "demo", installed by an unverified installer`},
	} {
		if got := notifyBanner("demo", tc.receipt); got != tc.want {
			t.Fatalf("%s: the banner says %q", tc.name, got)
		}
	}
	if installedBy(nil) != "unknown" {
		t.Fatal("status no longer shows unknown for a plugin without a receipt")
	}
	// A manual plugin through the real dispatch path.
	s, fake, clock := notifySupervisor(t)
	startDemo(t, s)
	drain(t, s, clock)
	calls := fake.calls(t)
	if len(calls) != 1 || calls[0][len(calls[0])-1] != `new background plugin "demo", installed by an unverified installer` {
		t.Fatalf("a plugin without a receipt got no unverified-installer banner: %q", calls)
	}
	if run := savedRun(t, s); run.NotificationState != notifyDispatched {
		t.Fatalf("notification state %q", run.NotificationState)
	}
}

// S19: the intent is durable with the first start, the host runs at once,
// and no banner work happens on the start path. A failed or timed-out
// child keeps the plugin running and charges nothing.
func TestPluginFirstStartNotificationBestEffort(t *testing.T) {
	t.Run("queued with the start", checkQueuedWithStart)
	for _, mode := range []string{"fail", "sleep", "loud"} {
		t.Run(mode, func(t *testing.T) { checkBadChildCharged(t, mode) })
	}
	t.Run("reserved before exec", checkReservedBeforeExec)
	t.Run("worker", checkWorkerDispatches)
	t.Run("interrupted replay", checkInterruptedReplay)
}

func checkQueuedWithStart(t *testing.T) {
	s, fake, _ := notifySupervisor(t)
	p := startDemo(t, s)
	run := savedRun(t, s)
	if !run.Notified || run.NotificationState != notifyQueued || run.State != runStarting || run.PID != p.host.cmd.Process.Pid {
		t.Fatalf("the first start did not save its intent with the start: %+v", run)
	}
	if len(fake.calls(t)) != 0 || len(s.notify.queue) != 1 {
		t.Fatal("the start path ran a banner child or did not offer the intent")
	}
}

// checkBadChildCharged: a failed, slow or loud child is bounded, recorded as
// failed, clears its reservation and leaves the plugin alone.
func checkBadChildCharged(t *testing.T, mode string) {
	t.Helper()
	s, fake, clock := notifySupervisor(t)
	fake.mode(t, mode)
	p := startDemo(t, s)
	began := time.Now()
	drain(t, s, clock)
	run := savedRun(t, s)
	if time.Since(began) > notifyRunLimit+2*time.Second {
		t.Fatalf("a %s child was not ended within its bound", mode)
	}
	if run.NotificationState != notifyFailed || run.NotificationReason != notifyReasonFailed {
		t.Fatalf("a %s child was recorded as %q", mode, run.NotificationState)
	}
	if p.host == nil || run.State != runStarting || len(run.Restarts) != 0 {
		t.Fatalf("a %s banner affected the plugin: %+v", mode, run)
	}
	if record, err := loadNotifyRecord(s.state); err != nil || record.InFlight != nil {
		t.Fatalf("the reservation was not cleared after a %s child: %v", mode, err)
	}
}

func checkReservedBeforeExec(t *testing.T) {
	s, _, clock := notifySupervisor(t)
	var before notifyRecord
	var loadErr error
	s.notify.run = func(text string, started func(pid int)) notifyOutcome {
		before, loadErr = loadNotifyRecord(s.state) // what is on disk at exec
		return runNotifyChild(text, started)
	}
	startDemo(t, s)
	drain(t, s, clock)
	if loadErr != nil || before.InFlight == nil || before.InFlight.Phase != "reserved" || before.InFlight.Controller != s.instance || len(before.Attempts) != 1 {
		t.Fatalf("the child ran before its reservation was durable: %+v %v", before, loadErr)
	}
}

func checkWorkerDispatches(t *testing.T) {
	s, fake, _ := notifySupervisor(t)
	p := startDemo(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	go s.notify.work(ctx)
	select {
	case res := <-s.notify.results:
		if res.installation != p.run.Installation || res.state != notifyDispatched || len(fake.calls(t)) != 1 {
			t.Fatalf("the worker result: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not dispatch the offered intent")
	}
	cancel()
	<-s.notify.done
}

// checkInterruptedReplay: the controller ended before it dispatched; a new
// generation replays the durable intent once.
func checkInterruptedReplay(t *testing.T) {
	s, fake, clock := notifySupervisor(t)
	startDemo(t, s)
	next := newNotifySupervisor(t, s.state)
	next.notify = testNotifier(s.state, next.instance, clock)
	p := next.supervisedFor("demo", nil, installationID(t, s.state, "demo"))
	next.offerQueued([]string{"demo"})
	drain(t, next, clock)
	if len(fake.calls(t)) != 1 || p.run.NotificationState != notifyDispatched {
		t.Fatalf("a queued intent was not replayed once: %d calls, %q", len(fake.calls(t)), p.run.NotificationState)
	}
}

// No banner on restart, backoff retry, plugins restart, enable after
// disable, or a new controller; a failed intent is not tried again.
func TestFirstStartNotificationNeverRepeats(t *testing.T) {
	s, fake, clock := notifySupervisor(t)
	p := startDemo(t, s)
	drain(t, s, clock)
	again := func(why string) {
		t.Helper()
		if p.host != nil {
			s.stopHost("demo", p, "restart_requested")
		}
		s.start("demo", p, p.binding)
		s.offerQueued([]string{"demo"})
		drain(t, s, clock)
		if n := len(fake.calls(t)); n != 1 {
			t.Fatalf("a banner was shown again after %s (%d banners)", why, n)
		}
	}
	again("an ordinary restart")
	s.stopHost("demo", p, "disabled")
	again("enable after disable")
	s.stopHost("demo", p, "killed")
	s.fail(p, time.Now(), "failed")
	again("a backoff retry")
	again("plugins restart")
	next := newNotifySupervisor(t, s.state)
	next.notify = testNotifier(s.state, next.instance, clock)
	s.stopAll("supervisor_stopped")
	p = startDemo(t, next)
	next.offerQueued([]string{"demo"})
	drain(t, next, clock)
	if n := len(fake.calls(t)); n != 1 {
		t.Fatalf("a banner was shown again after a controller restart (%d banners)", n)
	}
	// A failed intent stays failed in a new generation.
	p.run.NotificationState, p.run.NotificationReason = notifyFailed, notifyReasonFailed
	next.save("demo", p)
	last := newNotifySupervisor(t, s.state)
	last.notify = testNotifier(s.state, last.instance, clock)
	next.stopAll("supervisor_stopped")
	startDemo(t, last)
	last.offerQueued([]string{"demo"})
	if len(last.notify.queue) != 0 {
		t.Fatal("a failed intent was offered again in a new generation")
	}
}

// Q4: one child at a time, at least 10 s apart, at most 6 in any 60 s, a
// 60 s hold after a new generation, all on the monotonic clock.
func TestPluginNotificationBudget(t *testing.T) {
	wall := stateEpoch
	budget := func(mono time.Duration, mine ...time.Duration) *notifyBudget {
		return &notifyBudget{elapsed: func() time.Duration { return mono }, wall: func() time.Time { return wall }, mine: mine}
	}
	if budget(notifyWindow-time.Nanosecond).wait(nil) == 0 || budget(notifyWindow).wait(nil) != 0 {
		t.Fatal("the hold after a new generation is not exactly 60 s")
	}
	if budget(70*time.Second-time.Nanosecond, 60*time.Second).wait(nil) == 0 || budget(70*time.Second, 60*time.Second).wait(nil) != 0 {
		t.Fatal("the spacing is not exactly 10 s")
	}
	six := []time.Duration{60 * time.Second, 70 * time.Second, 80 * time.Second, 90 * time.Second, 100 * time.Second, 110 * time.Second}
	if budget(120*time.Second, six...).wait(nil) == 0 {
		t.Fatal("an attempt exactly 60 s ago was not counted")
	}
	if budget(120*time.Second+time.Nanosecond, six...).wait(nil) != 0 {
		t.Fatal("the window did not recover after 60 s")
	}
	// A forward wall step (+1h, 1 ms monotonic) frees nothing.
	b := budget(110*time.Second+time.Millisecond, six...)
	wall = stateEpoch.Add(time.Hour)
	if b.wait(nil) == 0 {
		t.Fatal("a forward clock step allowed a child early in one generation")
	}
	// The same after a clean restart: the stored UTC attempts look old, but
	// the new generation holds.
	stored := []time.Time{stateEpoch, stateEpoch.Add(10 * time.Second)}
	if budget(time.Millisecond).wait(stored) == 0 {
		t.Fatal("a forward clock step allowed a child early after a restart")
	}
	if budget(notifyWindow).wait(stored) != 0 {
		t.Fatal("a later real window did not recover")
	}
	// A backward step: attempts in the future still count.
	future := make([]time.Time, notifyMax)
	for i := range future {
		future[i] = wall.Add(time.Duration(i+1) * time.Minute)
	}
	if budget(notifyWindow).wait(future) == 0 {
		t.Fatal("attempts in the future were not counted after the clock went back")
	}
	t.Run("queue", checkNotifyQueue)
}

// checkNotifyQueue: at most 31 waiting plus one in work; a full queue keeps
// the intent queued and a later rescan offers it.
func checkNotifyQueue(t *testing.T) {
	s, _, clock := notifySupervisor(t)
	for range 31 { // the contract cap: 31 waiting plus one in work
		if !s.notify.offer(notifyIntent{name: "x", installation: model.NewUUID()}) {
			t.Fatal("the queue refused an intent before it was full")
		}
	}
	p := startDemo(t, s)
	if s.offered[p.run.Installation] || savedRun(t, s).NotificationState != notifyQueued {
		t.Fatal("a full queue lost the intent or marked it offered")
	}
	for len(s.notify.queue) > 0 {
		<-s.notify.queue
	}
	s.offerQueued([]string{"demo"})
	drain(t, s, clock)
	if savedRun(t, s).NotificationState != notifyDispatched {
		t.Fatal("the rescan did not offer a queued intent after the queue had room")
	}
}

// The text and the label travel as one literal argv item; the script is
// byte-identical every time. The child is bounded with a group signal
// sent while the leader is unreaped.
func TestPluginNotificationArgvAndBounds(t *testing.T) {
	fake := useNotifyFixture(t)
	before := postReapSignals()
	hostile := strings.Repeat("x", 300) + "\" & do shell script \"touch /tmp/pwned\" & \"\\\n\tend run"
	for _, text := range []string{"plain", hostile} {
		if !runNotifyChild(text, func(int) {}).ok {
			t.Fatal("the fixture child failed")
		}
	}
	// A child that already exited: the group signal must still come while
	// its leader is unreaped. The TestMain guard refuses (never sends) a
	// signal after the reap and counts it.
	if n := postReapSignals() - before; n != 0 {
		t.Fatalf("the group signal came after the leader was reaped: %d refused", n)
	}
	for _, call := range fake.calls(t) {
		want := []string{"-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run", "Zalki"}
		if len(call) != len(want)+1 || !slices.Equal(call[:len(want)], want) || (call[len(want)] != "plain" && call[len(want)] != hostile) {
			t.Fatalf("the banner text did not travel as one literal argv item: %q", call)
		}
	}
	fake.mode(t, "sleep")
	began := time.Now()
	ok := runNotifyChild("slow", func(int) {}).ok
	took := time.Since(began)
	if ok || took > notifyRunLimit+2*time.Second {
		t.Fatal("a slow child was not ended within its bound")
	}
}

// Only the real default installation sends banners: macOS, the OS account
// home and a state that resolves to its default state (also when given with
// --state or through a link, R7-M4). Anything else records no intent and
// calls no notifier.
func TestPluginNotificationCustomStateSendsNothing(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".local", "state", "zalki")
	if err := os.MkdirAll(filepath.Join(state, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldPlatform, oldHome := backgroundPlatform, loginItemAccountHome
	t.Cleanup(func() { backgroundPlatform, loginItemAccountHome = oldPlatform, oldHome })
	backgroundPlatform, loginItemAccountHome = "darwin", func() string { return home }
	if n := notifierFor(state, home, model.NewUUID()); n == nil || n.dir == nil {
		t.Fatal("the real default installation has no notifier")
	}
	alias := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(state, alias); err != nil {
		t.Fatal(err)
	}
	if n := notifierFor(alias, home, model.NewUUID()); n == nil {
		t.Fatal("the default state given through a link has no notifier")
	}
	if notifierFor(t.TempDir(), home, model.NewUUID()) != nil {
		t.Fatal("a custom state got a notifier")
	}
	if notifierFor(filepath.Join(t.TempDir(), "missing"), home, model.NewUUID()) != nil {
		t.Fatal("a state that cannot be resolved got a notifier")
	}
	other := t.TempDir()
	loginItemAccountHome = func() string { return other } // HOME is not the account home
	if notifierFor(state, home, model.NewUUID()) != nil {
		t.Fatal("a custom HOME got a notifier")
	}
	loginItemAccountHome = func() string { return home }
	backgroundPlatform = "linux"
	if notifierFor(state, home, model.NewUUID()) != nil {
		t.Fatal("another platform got a notifier")
	}
	s, fake, _ := notifySupervisor(t)
	s.notify = notifierFor(s.state, s.home, s.instance)
	if s.notify != nil {
		t.Fatal("a custom-state controller has a notifier")
	}
	startDemo(t, s)
	s.offerQueued([]string{"demo"})
	if run := savedRun(t, s); run.Notified || run.NotificationState != "" || len(fake.calls(t)) != 0 {
		t.Fatal("a custom-state controller recorded a notification intent")
	}
	if _, err := os.Lstat(notifyPath(s.state)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a custom-state controller wrote notification state")
	}
}

// A reinstall under the same NAME while the old intent waits: the old
// UUID is never announced with the new receipt, the new one gets its own.
func TestPluginNotificationReinstallKeepsUUIDs(t *testing.T) {
	s, fake, clock := notifySupervisor(t)
	old := startDemo(t, s).run.Installation
	s.stopHost("demo", s.plugins["demo"], "removed")
	for _, args := range [][]string{{"remove", "demo"}, {"add", "demo", "--exec", feedFixture(t)}} {
		if raw, err := pluginOwnerLocal(t, s.state, args...); err != nil {
			t.Fatal(string(raw), err)
		}
	}
	if wait, res := s.notify.try(<-s.notify.queue); wait != 0 || res != nil || len(fake.calls(t)) != 0 {
		t.Fatal("an intent of a removed installation was announced under the new one")
	}
	p := startDemo(t, s)
	if p.run.Installation == old || p.run.NotificationState != notifyQueued {
		t.Fatal("the new installation has no intent of its own")
	}
	drain(t, s, clock)
	if len(fake.calls(t)) != 1 || savedRun(t, s).NotificationState != notifyDispatched {
		t.Fatal("the new installation was not announced once")
	}
}

// Each owner line, with its condition (the truth rule).
func TestPluginNotificationLines(t *testing.T) {
	run := pluginRunState{Notified: true}
	for _, tc := range []struct {
		state, reason, want string
	}{
		{notifyDispatched, "", ""},
		{notifyQueued, "", ""},
		{notifyFailed, notifyReasonFailed, "; notification failed"},
		{notifyQueued, notifyReasonUnknown, "; notifications paused"},
		{notifyQueued, notifyReasonBadState, "; notifications paused"},
	} {
		run.NotificationState, run.NotificationReason = tc.state, tc.reason
		if got := notifySuffix(run, true); got != tc.want || notifySuffix(run, false) != "" {
			t.Fatalf("list suffix for %s/%s: %q", tc.state, tc.reason, got)
		}
	}
	if notifyOwnerCopy != "The first time a background plugin starts, Zalki queues one best-effort macOS notification; it may not appear, and a crash may repeat it. Zalki writes its notification bookkeeping only under its private state. The title and text go to the macOS notification service through osascript. Zalki makes no network request for it." {
		t.Fatal("the owner copy is not the reviewed text")
	}
	if !strings.HasSuffix(newPluginSuperviseCommand(&commandOptions{}).Long, " "+notifyOwnerCopy) {
		t.Fatal("the supervise help does not carry the banner owner copy")
	}
	store, _ := pluginOwnerFixture(t)
	if err := os.MkdirAll(filepath.Join(store.Dir, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	live := true
	oldLive := controllerLive
	controllerLive = func(string) bool { return live }
	t.Cleanup(func() { controllerLive = oldLive })
	if notes := notifyNotes(store); len(notes) != 0 {
		t.Fatalf("doctor notes without a cause: %q", notes)
	}
	record := notifyRecord{Schema: 1, Attempts: []time.Time{stateEpoch}, InFlight: &notifyInFlight{Token: model.NewUUID(), Controller: model.NewUUID(), Phase: "reserved"}}
	writeNotifyRecord(t, store.Dir, record)
	if notes := notifyNotes(store); len(notes) != 0 {
		t.Fatalf("a live controller's own dispatch was named unfinished: %q", notes)
	}
	live = false
	if notes := notifyNotes(store); !slices.Equal(notes, []string{"Zalki notifications are paused: an earlier notification did not finish. Plugins are not affected.", fmt.Sprintf(noteNotifyResume, "gui/"+strconv.Itoa(os.Getuid())+"/dev.zalki.plugins", shellQuote(filepath.Join(store.Dir, "plugins", notifyFileName)))}) {
		t.Fatalf("doctor unfinished note: %q", notes)
	}
	if err := os.WriteFile(notifyPath(store.Dir), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if notes := notifyNotes(store); !slices.Equal(notes, []string{"Zalki notifications are paused: the notification state file is not valid. Plugins are not affected.", fmt.Sprintf(noteNotifyResume, "gui/"+strconv.Itoa(os.Getuid())+"/dev.zalki.plugins", shellQuote(filepath.Join(store.Dir, "plugins", notifyFileName)))}) {
		t.Fatalf("doctor invalid note: %q", notes)
	}
	checkNotifyRecordKept(t, store)
}

// An invalid record is kept byte-identical and pauses only notifications.
func checkNotifyRecordKept(t *testing.T, store bus.Store) {
	t.Helper()
	before, _ := os.ReadFile(notifyPath(store.Dir))
	n := testNotifier(store.Dir, model.NewUUID(), &notifyClock{mono: notifyWindow, wall: stateEpoch})
	if _, res := n.try(notifyIntent{name: "demo", installation: model.NewUUID()}); res == nil || res.reason != notifyReasonBadState {
		t.Fatal("an invalid notification record did not pause with its reason")
	}
	if after, _ := os.ReadFile(notifyPath(store.Dir)); !bytes.Equal(before, after) {
		t.Fatal("an invalid notification record was changed")
	}
}

func writeNotifyRecord(t *testing.T, state string, record notifyRecord) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err == nil {
		err = os.WriteFile(notifyPath(state), raw, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
}
