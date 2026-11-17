package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

func useNotifyKill(t *testing.T, kill func(int, syscall.Signal) error) {
	t.Helper()
	old := notifyKill
	notifyKill = kill
	t.Cleanup(func() { notifyKill = old })
}

// 7D3-S1: a group cleanup without positive evidence keeps the reservation
// and pauses every later dispatch; a clear-write error (before or after the
// record changed on disk) reaches the owner state as a pause.
func TestPluginNotificationCleanupUncertaintyFences(t *testing.T) {
	t.Run("group error", checkGroupErrorFences)
	for _, late := range []bool{false, true} {
		checkClearErrorFences(t, late)
	}
}

// checkGroupErrorFences: a group signal error keeps the reservation.
func checkGroupErrorFences(t *testing.T) {
	s, fake, clock := notifySupervisor(t)
	useNotifyKill(t, func(int, syscall.Signal) error { return syscall.EIO })
	p := startDemo(t, s)
	drain(t, s, clock)
	record, err := loadNotifyRecord(s.state)
	if err != nil || record.InFlight == nil || p.run.NotificationReason != notifyReasonUnknown {
		t.Fatalf("an unresolved group cleanup cleared the reservation: %+v %v %q", record.InFlight, err, p.run.NotificationReason)
	}
	clock.mono += time.Hour
	calls := firstDispatchSeen(t, fake)
	if _, res := s.notify.try(notifyIntent{name: "demo", installation: p.run.Installation}); res == nil || res.reason != notifyReasonUnknown || len(fake.calls(t)) != calls {
		t.Fatal("a dispatch followed an unresolved cleanup")
	}
}

// checkClearErrorFences: the clear write fails, before (late false) or
// after (late true) the record changed on disk.
func checkClearErrorFences(t *testing.T, late bool) {
	t.Helper()
	s, fake, clock := notifySupervisor(t)
	writes := 0
	s.notify.write = func(dir *os.File, raw []byte) error {
		writes++
		if writes != 3 { // reserve, started, then the clear
			return writeNotifyAt(dir, raw)
		}
		if late {
			if err := writeNotifyAt(dir, raw); err != nil {
				return err
			}
		}
		return errors.New("clear write failed")
	}
	p := startDemo(t, s)
	drain(t, s, clock)
	if p.run.NotificationState != notifyQueued || p.run.NotificationReason != notifyReasonUnknown || notifySuffix(p.run, true) != listNotifyPaused {
		t.Fatalf("a clear error was hidden from the owner (published=%t): %q/%q", late, p.run.NotificationState, p.run.NotificationReason)
	}
	clock.mono += time.Hour
	calls := firstDispatchSeen(t, fake)
	if _, res := s.notify.try(notifyIntent{name: "demo", installation: p.run.Installation}); res == nil || len(fake.calls(t)) != calls {
		t.Fatalf("a dispatch followed a clear error (published=%t)", late)
	}
}

// 7D3-S2: a leader that left its original group survives the group signal
// (ESRCH); the unconditional kill of the original handle still bounds Wait.
func TestPluginNotificationLeaderKillIsUnconditional(t *testing.T) {
	fake := useNotifyFixture(t)
	fake.mode(t, "leave")
	began := time.Now()
	out := runNotifyChild("leave", func(int) {})
	if _, err := os.Stat(filepath.Join(fake.dir, "left")); err != nil {
		t.Fatal("the fixture did not leave its group")
	}
	if out.ok || time.Since(began) > notifyRunLimit+2*time.Second {
		t.Fatalf("the leader that left its group was not bounded (%s)", time.Since(began))
	}
}

// 7D3-S3: the watchdog runs while the started save is still blocked, and a
// child that closes its output early may still finish within its bound.
func TestPluginNotificationWatchdogAndExit(t *testing.T) {
	fake := useNotifyFixture(t)
	fake.mode(t, "sleep")
	began := time.Now()
	var killedAt time.Duration
	guarded := notifyKill // the TestMain guard: never a signal after the reap
	useNotifyKill(t, func(pid int, sig syscall.Signal) error {
		if killedAt == 0 {
			killedAt = time.Since(began)
		}
		return guarded(pid, sig)
	})
	runNotifyChild("slow", func(int) { time.Sleep(notifyRunLimit + time.Second) })
	if killedAt == 0 || killedAt > notifyRunLimit+500*time.Millisecond {
		t.Fatalf("the watchdog waited for the started save (group signal at %s)", killedAt)
	}
	if err := os.Remove(filepath.Join(fake.dir, "sleep")); err != nil {
		t.Fatal(err)
	}
	fake.mode(t, "quiet")
	if out := runNotifyChild("quiet", func(int) {}); !out.ok || !out.cleaned {
		t.Fatalf("a child that closed its output was ended before it exited: %+v", out)
	}
}

// 7D3-S4: shutdown while the worker is sending a result on a full channel
// still ends, and the result is saved.
func TestPluginNotificationShutdownDrainsResults(t *testing.T) {
	s, _, _ := notifySupervisor(t)
	s.notify.results = make(chan notifyResult) // no buffer: a send waits for the loop
	inRun, release := make(chan struct{}), make(chan struct{})
	s.notify.run = func(_ string, started func(pid int)) notifyOutcome {
		close(inRun)
		<-release
		return notifyOutcome{ok: true, cleaned: true}
	}
	startDemo(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan error, 1)
	go func() { ended <- s.runWithNotifier(ctx) }()
	select {
	case <-inRun:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not start its dispatch")
	}
	cancel()
	time.Sleep(200 * time.Millisecond) // the controller loop has ended
	close(release)
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown deadlocked on the result channel")
	}
	if run := savedRun(t, s); run.NotificationState != notifyDispatched {
		t.Fatalf("the last result was not saved at shutdown: %q", run.NotificationState)
	}
}

// 7D3-S5: notification writes stay in the plugins folder opened at start,
// even after the path is replaced by a link; a reservation changed on disk
// during the dispatch is kept byte-identical and fences.
func TestPluginNotificationRecordIsAnchored(t *testing.T) {
	t.Run("replaced parent", func(t *testing.T) {
		s, _, clock := notifySupervisor(t)
		startDemo(t, s)
		plugins := filepath.Join(s.state, "plugins")
		moved, sentinel := plugins+".moved", t.TempDir()
		// The path is replaced after the reservation, while the child runs:
		// the started and clear writes must still use the opened folder.
		s.notify.run = func(text string, started func(pid int)) notifyOutcome {
			if err := os.Rename(plugins, moved); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(sentinel, plugins); err != nil {
				t.Error(err)
			}
			return runNotifyChild(text, started)
		}
		t.Cleanup(func() { _ = os.Remove(plugins); _ = os.Rename(moved, plugins) })
		drain(t, s, clock)
		if _, err := os.Lstat(filepath.Join(sentinel, notifyFileName)); err == nil {
			t.Fatal("a notification write followed a replaced plugins folder")
		}
		if _, err := os.Lstat(filepath.Join(moved, notifyFileName)); err != nil {
			t.Fatal("the record was not written in the folder opened at start")
		}
	})
	t.Run("changed reservation", func(t *testing.T) {
		s, fake, clock := notifySupervisor(t)
		foreign := notifyRecord{Schema: 1, Attempts: []time.Time{stateEpoch}, InFlight: &notifyInFlight{Token: model.NewUUID(), Controller: model.NewUUID(), Phase: "reserved"}}
		s.notify.run = func(text string, started func(pid int)) notifyOutcome {
			out := runNotifyChild(text, started)
			writeNotifyRecord(t, s.state, foreign)
			return out
		}
		p := startDemo(t, s)
		drain(t, s, clock)
		want, err := json.Marshal(foreign)
		if err != nil {
			t.Fatal(err)
		}
		if got := mustRead(t, notifyPath(s.state)); !bytes.Equal(got, want) || p.run.NotificationReason != notifyReasonUnknown {
			t.Fatal("a changed reservation was overwritten")
		}
		clock.mono += time.Hour
		calls := firstDispatchSeen(t, fake)
		if _, res := s.notify.try(notifyIntent{name: "demo", installation: p.run.Installation}); res == nil || len(fake.calls(t)) != calls {
			t.Fatalf("a dispatch followed a changed reservation: %+v", res)
		}
	})
}

// 7D3-S8: reinstalling the same NAME again and again keeps the offered set
// as small as the current plugin list.
func TestPluginNotificationOfferedIsBounded(t *testing.T) {
	s, _, clock := notifySupervisor(t)
	for range 5 {
		p := startDemo(t, s)
		drain(t, s, clock)
		s.stopHost("demo", p, "removed")
		ownerRun(t, s.state, "remove", "demo")
		ownerRun(t, s.state, "add", "demo", "--exec", feedFixture(t))
	}
	startDemo(t, s)
	s.pruneOffered() // a scan prunes, then offers (ZAL-68)
	s.offerQueued([]string{"demo"})
	if len(s.offered) > 1 {
		t.Fatalf("offered bookkeeping grew with reinstalls: %d", len(s.offered))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// R2-M2: the original kill fails and the leader (outside its old group) is
// still alive: the runner still returns within its bounds, and the result is
// not cleaned, so the reservation stays and dispatch is fenced.
func TestPluginNotificationUnjoinedLeaderFences(t *testing.T) {
	fake := useNotifyFixture(t)
	fake.mode(t, "leave")
	old := notifyKillLeader
	notifyKillLeader = func(*os.Process) error { return syscall.EPERM }
	t.Cleanup(func() { notifyKillLeader = old })
	pids := make(chan int, 1)
	began := time.Now()
	out := runNotifyChild("leave", func(p int) { pids <- p })
	if took := time.Since(began); took > notifyRunLimit+notifyJoinLimit+1500*time.Millisecond {
		t.Fatalf("an unjoined leader held the runner (%s)", took)
	}
	if out.cleaned {
		t.Fatal("an unjoined leader was counted as cleaned")
	}
	// The self-bounded fixture ends by itself (8 s); the runner's own Wait
	// then reaps it. Nothing is signalled here.
	pid := <-pids
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := procwatch.Read(pid); processAbsent(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the fixture leader did not end by itself")
}

// R2-M3: a FIFO at the record path cannot block the worker: it is refused
// as an invalid state and kept.
func TestPluginNotificationFIFORecordDoesNotBlock(t *testing.T) {
	s, _, _ := notifySupervisor(t)
	if err := unix.Mkfifo(notifyPath(s.state), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan *notifyResult, 1)
	go func() {
		_, res := s.notify.try(notifyIntent{name: "demo", installation: installationID(t, s.state, "demo")})
		done <- res
	}()
	select {
	case res := <-done:
		if res == nil || res.reason != notifyReasonBadState {
			t.Fatalf("a FIFO record was not refused as an invalid state: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO record blocked the worker")
	}
	if info, err := os.Lstat(notifyPath(s.state)); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("the FIFO record was not kept")
	}
}

// The live-controller probe that status, list and doctor use cannot be held
// by a FIFO at the lock path.
func TestPluginControllerProbeDoesNotBlock(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(state, "plugins", ".supervisor.lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { done <- supervisorRunning(state) }()
	select {
	case live := <-done:
		if live {
			t.Fatal("a FIFO was read as a live controller")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO lock path blocked the controller probe")
	}
}

// R3-M2: Wait fails without a reap (the seam returns at once; the real Wait
// still happens later, once). The runner must not count that as cleaned.
func TestPluginNotificationWaitErrorIsNotCleaned(t *testing.T) {
	useNotifyFixture(t)
	guarded := notifyWait
	reaped := make(chan struct{})
	notifyWait = func(cmd *exec.Cmd) (*os.ProcessState, error) {
		go func() { // the eventual sole real join of the bounded original
			_, _ = guarded(cmd)
			close(reaped)
		}()
		return nil, errors.New("wait failed before a reap")
	}
	t.Cleanup(func() { notifyWait = guarded })
	if out := runNotifyChild("wait error", func(int) {}); out.cleaned {
		t.Fatal("a wait error without a reap was counted as cleaned")
	}
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("the original child was never joined")
	}
}

// firstDispatchSeen is the precondition of every "no second dispatch" check:
// the first child really ran (its call is recorded), so a missing first
// dispatch cannot make the check pass by itself. It returns the call count.
func firstDispatchSeen(t *testing.T, fake notifyFake) int {
	t.Helper()
	calls := len(fake.calls(t))
	if calls != 1 {
		t.Fatalf("precondition: the first banner child did not run (%d calls)", calls)
	}
	return calls
}
