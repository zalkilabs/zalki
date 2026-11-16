package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// noNotifier is the banner executable of every test binary unless a test
// sets its own fixture: it does not exist, so nothing real can run.
const noNotifier = "/nonexistent/zalki-test-notifier"

// notifyHelperEnv makes the test binary act as a controller that dies in
// the middle of a banner dispatch (TestPluginNotificationInFlightFence).
const notifyHelperEnv = "ZALKI_NOTIFY_FENCE_HELPER"

func TestMain(m *testing.M) {
	notifyExecutable = noNotifier
	guardNotifySignals()
	if dir := os.Getenv(notifyHelperEnv); dir != "" {
		os.Exit(runNotifyHelper(dir))
	}
	os.Exit(m.Run())
}

// No test binary run may reach the real osascript.
func TestPluginNotifyTestsRunOnlyTheFixture(t *testing.T) {
	if notifyExecutable != noNotifier || filepath.Base(notifyExecutable) == "osascript" {
		t.Fatalf("a test binary would run the banner executable %q", notifyExecutable)
	}
}

// runNotifyHelper starts one dispatch for the plugin and stops for ever
// right after the started phase is saved; the parent then kills it.
func runNotifyHelper(dir string) int {
	notifyExecutable = filepath.Join(dir, "notifier")
	clock := &notifyClock{mono: notifyWindow, wall: time.Now()}
	n := testNotifier(os.Getenv(notifyHelperEnv+"_STATE"), os.Getenv(notifyHelperEnv+"_CONTROLLER"), clock)
	n.run = func(text string, started func(pid int)) notifyOutcome {
		return runNotifyChild(text, func(pid int) {
			started(pid)
			_ = os.WriteFile(filepath.Join(dir, "helper-started"), nil, 0o600)
			time.Sleep(time.Minute) // the parent kills this process now
		})
	}
	n.try(notifyIntent{name: "demo", installation: os.Getenv(notifyHelperEnv + "_ID")})
	return 3 // never reached while the parent kills it
}

// The test-wide signal guard: once a banner child's Wait has returned, its
// group number is no longer ours, so a group signal to it is REFUSED before
// any real signal and recorded. No test or fault can reach a reused group.
var (
	reapMu     sync.Mutex
	reaped     = map[int]bool{}
	postReap   []int
	realNotify = notifyKill
)

func guardNotifySignals() {
	wait := notifyWait
	notifyWait = func(cmd *exec.Cmd) (*os.ProcessState, error) {
		state, err := wait(cmd)
		if state != nil {
			reapMu.Lock()
			reaped[cmd.Process.Pid] = true
			reapMu.Unlock()
		}
		return state, err
	}
	notifyKill = func(pid int, sig syscall.Signal) error {
		reapMu.Lock()
		refused := pid < 0 && reaped[-pid]
		if refused {
			postReap = append(postReap, -pid)
		}
		reapMu.Unlock()
		if refused {
			return syscall.ESRCH // what the kernel says for a group that is gone
		}
		return realNotify(pid, sig)
	}
}

// postReapSignals counts the refused group signals so far.
func postReapSignals() int {
	reapMu.Lock()
	defer reapMu.Unlock()
	return len(postReap)
}

// The guard itself: a group signal for a reaped banner child is refused and
// counted, never sent. Signal 0 is used, so even a broken guard sends
// nothing that changes a process.
func TestPluginNotifySignalGuardRefusesAfterReap(t *testing.T) {
	useNotifyFixture(t)
	pids := make(chan int, 1)
	if !runNotifyChild("guard", func(pid int) { pids <- pid }).ok {
		t.Fatal("the fixture child failed")
	}
	before := postReapSignals()
	if err := notifyKill(-<-pids, 0); !errors.Is(err, syscall.ESRCH) || postReapSignals() != before+1 {
		t.Fatal("the test signal guard did not refuse a group signal after the reap")
	}
}
