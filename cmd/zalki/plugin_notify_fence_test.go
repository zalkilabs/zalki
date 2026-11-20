package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// A controller dies (SIGKILL through its owned handle) while its banner
// child runs. Several new generations see its in-flight record: none starts
// a second child, none clears or signals anything, plugins still start, and
// the pause is named. The fixture child ends by itself within its bound.
func TestPluginNotificationInFlightFence(t *testing.T) {
	s, fake, clock := notifySupervisor(t)
	fake.mode(t, "sleep") // the child lives 5 s, then writes its slept marker
	id := installationID(t, s.state, "demo")
	helper := startNotifyHelper(t, fake.dir, s.state, id)
	before, err := os.ReadFile(notifyPath(s.state))
	record, loadErr := loadNotifyRecord(s.state)
	if err != nil || loadErr != nil || record.InFlight == nil || record.InFlight.Phase != "started" {
		t.Fatalf("the dying controller left no started reservation: %v %v", err, loadErr)
	}
	if err := helper.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	awaitNotifyCalls(t, fake, 1) // the orphaned child runs on by itself
	for range 3 {
		next := newNotifySupervisor(t, s.state)
		next.notify = testNotifier(s.state, next.instance, clock)
		p := startDemo(t, next)
		next.offerQueued([]string{"demo"})
		drain(t, next, clock)
		if p.host == nil || p.run.NotificationState != notifyQueued || p.run.NotificationReason != notifyReasonUnknown {
			t.Fatalf("a new generation did not pause with notification_dispatch_unknown: %+v", p.run)
		}
		if after, _ := os.ReadFile(notifyPath(s.state)); !bytes.Equal(before, after) {
			t.Fatal("a new generation cleared or changed the earlier in-flight record")
		}
		if n := len(fake.calls(t)); n != 1 {
			t.Fatalf("a second banner child started after the controller died (%d)", n)
		}
		next.stopAll("supervisor_stopped")
	}
	awaitSlept(t, fake.dir)
}

// startNotifyHelper starts the test binary as a controller that stops
// right after its child's started phase is saved.
func startNotifyHelper(t *testing.T, dir, state, id string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), self, "-test.run=^$")
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin", notifyHelperEnv + "=" + dir, notifyHelperEnv + "_STATE=" + state,
		notifyHelperEnv + "_CONTROLLER=" + model.NewUUID(), notifyHelperEnv + "_ID=" + id}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Kill only our own unreaped helper; its banner child has its own group
	// and ends by itself.
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "helper-started")); err == nil {
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the helper controller never started its banner child")
	return nil
}

// awaitSlept waits for the self-bounded fixture child to finish.
func awaitSlept(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if paths, _ := filepath.Glob(filepath.Join(dir, "slept.*")); len(paths) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the fixture banner child did not end by itself")
}

func awaitNotifyCalls(t *testing.T, fake notifyFake, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.calls(t)) == n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the fixture banner child recorded %d calls, want %d", len(fake.calls(t)), n)
}
