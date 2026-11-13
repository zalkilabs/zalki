package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// supervisorFixture installs and enables feed plugins under names in a
// fresh state, with the real binary.
func supervisorFixture(t *testing.T, binary string, names ...string) string {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	for _, name := range names {
		for _, args := range [][]string{{"add", name, "--exec", crashableFixture(t, feedFixtureScript)}, {"configure", name, "--scope", "all"}, {"grant", name, "--status"}, {"enable", name}} {
			if output, err := pluginOwnerCLI(t, binary, store.Dir, args...); err != nil {
				t.Fatal(args, len(output), err)
			}
		}
	}
	return store.Dir
}

// hostCrashWatcher makes a fixture plugin kill its own host (its live
// parent, $PPID from the kernel) when the test creates die-host in the
// plugin's data folder: a host crash without the test signalling any PID
// it read from a file.
// The marker is moved away first, so each marker crashes the host once.
// Without a marker the watcher expires after 120 s and signals nothing.
const hostCrashWatcher = "( i=0; while [ ! -f die-host ] && [ $i -lt 1200 ]; do /bin/sleep 0.1; i=$((i+1)); done; [ -f die-host ] && /bin/mv die-host die-host.done && /bin/kill -9 $PPID ) </dev/null >/dev/null 2>&1 &\n"

func crashableFixture(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crashable-fixture")
	script = strings.Replace(script, "/bin/mkdir -p events || exit 2\n", "/bin/mkdir -p events || exit 2\n"+hostCrashWatcher, 1)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// crashHost asks name's fixture plugin to kill its host.
func crashHost(t *testing.T, state, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, "plugins", name+".data", "die-host"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// startSupervisor runs `zalki plugins supervise` in the foreground with a
// private HOME and the explicit state.
func startSupervisor(t *testing.T, binary, state string) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), binary, "--state", state, "plugins", "supervise")
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	t.Cleanup(func() {
		if t.Failed() {
			// The controller's own diagnostics: codes and names only.
			raw, _ := os.ReadFile(filepath.Join(state, "plugins", ".supervisor.log"))
			t.Logf("supervisor stderr: %s\nsupervisor log: %s", stderr.String(), raw)
		}
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done, exited := make(chan error, 1), make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		// SIGTERM to the owned controller: it stops and waits for every
		// host group, so nothing writes under state after the test.
		// Kill is only the fallback.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})
	return cmd, done
}

// stubbornWriter makes a fixture plugin start a writer that ignores
// SIGTERM and counts into beat in its data folder every 50 ms, so it lives
// until its group is killed. It expires after 120 s.
const stubbornWriter = "( trap '' TERM; i=0; while [ $i -lt 2400 ]; do i=$((i+1)); echo $i > beat; /bin/sleep 0.05; done ) </dev/null >/dev/null 2>&1 &\n"

// The supervisor cleanup leaves no plugin writing under state: the next
// cleanup (t.TempDir) must find a quiet folder. This check is registered
// first, so it runs after the supervisor's cleanup.
func TestPluginSupervisorCleanupLeavesNoWriter(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	store, _ := pluginOwnerFixture(t)
	script := strings.Replace(feedFixtureScript, "/bin/mkdir -p events || exit 2\n", "/bin/mkdir -p events || exit 2\n"+stubbornWriter, 1)
	// After its input ends the plugin waits, so the host's stop holds the
	// group (and the writer) for its full bound.
	script = strings.TrimSuffix(script, "done\n") + "done\n/bin/sleep 3\n"
	path := filepath.Join(t.TempDir(), "writer-fixture")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "alpha", "--exec", path}, {"configure", "alpha", "--scope", "all"}, {"grant", "alpha", "--status"}, {"enable", "alpha"}} {
		if output, err := pluginOwnerCLI(t, binary, store.Dir, args...); err != nil {
			t.Fatal(args, len(output), err)
		}
	}
	beat := filepath.Join(store.Dir, "plugins", "alpha.data", "beat")
	t.Cleanup(func() {
		before, _ := os.ReadFile(beat)
		time.Sleep(time.Second)
		if after, _ := os.ReadFile(beat); string(after) != string(before) {
			t.Errorf("a plugin still wrote under state after the supervisor cleanup: %q then %q", before, after)
		}
	})
	startSupervisor(t, binary, store.Dir)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if raw, err := os.ReadFile(beat); err == nil && len(raw) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fixture plugin did not start writing")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readRunState(t *testing.T, state, name string) pluginRunState {
	t.Helper()
	var run pluginRunState
	if id := installationID(t, state, name); id != "" {
		run, _ = loadRunState(state, name, id)
	}
	return run
}

func installationID(t *testing.T, state, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(state, "plugins", name+".json"))
	if err != nil {
		return ""
	}
	var record struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &record)
	return record.ID
}

func awaitRunState(t *testing.T, state, name string, ok func(pluginRunState) bool) pluginRunState {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if run := readRunState(t, state, name); ok(run) {
			return run
		}
		time.Sleep(100 * time.Millisecond)
	}
	run := readRunState(t, state, name)
	t.Fatalf("%s never reached the expected run state: state %s reason %s", name, run.State, run.ReasonCode)
	return run
}

func running(run pluginRunState) bool {
	return run.State == runRunning && run.PID > 0 && run.PluginPID > 0
}

func hostProcessGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// S01: two real host/plugin pairs run in separate host processes. Killing
// one host is counted and restarted after its backoff; the other plugin
// keeps the same live host and plugin throughout.
func TestPluginSupervisorRealChildrenCLI(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := supervisorFixture(t, binary, "alpha", "beta")
	startSupervisor(t, binary, state)
	alpha := awaitRunState(t, state, "alpha", running)
	beta := awaitRunState(t, state, "beta", running)
	if alpha.PID == beta.PID || alpha.PluginPID == beta.PluginPID {
		t.Fatal("two plugins share one host process")
	}
	crashHost(t, state, "alpha")
	restarted := awaitRunState(t, state, "alpha", func(run pluginRunState) bool {
		return running(run) && run.PID != alpha.PID && len(run.Restarts) == 1
	})
	if !hostProcessGone(alpha.PluginPID) {
		t.Fatal("the killed host's plugin outlived its group cleanup")
	}
	if now := readRunState(t, state, "beta"); !running(now) || now.PID != beta.PID || now.PluginPID != beta.PluginPID || hostProcessGone(beta.PluginPID) {
		t.Fatal("killing one host disturbed the other plugin")
	}
	if restarted.ReasonCode != "killed" {
		t.Fatalf("the killed host was recorded as %q", restarted.ReasonCode)
	}
}

// S14/S22: an orderly stop (SIGTERM) ends every host and plugin, records a
// clean stop without counting it, and exits 0. A second supervisor for the
// same state exits 0 at once while the first runs.
func TestPluginSupervisorForegroundAndStopCLI(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := supervisorFixture(t, binary, "alpha")
	cmd, done := startSupervisor(t, binary, state)
	run := awaitRunState(t, state, "alpha", running)
	second := exec.CommandContext(t.Context(), binary, "--state", state, "plugins", "supervise")
	second.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	if err := second.Run(); err != nil {
		t.Fatal("a second supervisor did not exit 0 at once", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("an orderly stop did not exit 0", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop within its bound")
	}
	if !hostProcessGone(run.PID) || !hostProcessGone(run.PluginPID) {
		t.Fatal("a host or plugin outlived the supervisor's stop")
	}
	after := readRunState(t, state, "alpha")
	if after.State != runStopped || after.ReasonCode != "supervisor_stopped" || len(after.Restarts) != 0 {
		t.Fatalf("the orderly stop was recorded as %s/%s with %d failures", after.State, after.ReasonCode, len(after.Restarts))
	}
}

// S12: a new session changes membership only and never restarts the host;
// a scope change is a new binding: the host is replaced, not counted.
func TestPluginSupervisorBindingAndMembership(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := supervisorFixture(t, binary, "alpha")
	startSupervisor(t, binary, state)
	first := awaitRunState(t, state, "alpha", running)
	store, err := bus.NewStore(state, adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register("codex", "00000000-0000-4000-8000-000000000002", t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(supervisePoll + time.Second)
	if now := readRunState(t, state, "alpha"); !running(now) || now.PID != first.PID {
		t.Fatal("a membership change restarted the host")
	}
	if output, err := pluginOwnerCLI(t, binary, state, "configure", "alpha", "--scope", "none"); err != nil {
		t.Fatal(len(output), err)
	}
	replaced := awaitRunState(t, state, "alpha", func(run pluginRunState) bool { return running(run) && run.PID != first.PID })
	if len(replaced.Restarts) != 0 || replaced.BackoffStep != 0 {
		t.Fatalf("a binding change was counted: %d failures, step %d", len(replaced.Restarts), replaced.BackoffStep)
	}
}

// S15: a lease someone else holds stops the start without a crash count or
// backoff; once the lease is free the plugin starts.
func TestPluginSupervisorLeaseHeldIsNotACrash(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := supervisorFixture(t, binary, "alpha")
	lease, err := pluginfeed.OpenPluginLease(filepath.Join(state, "plugins", "alpha.lock"))
	if err != nil {
		t.Fatal(err)
	}
	startSupervisor(t, binary, state)
	held := awaitRunState(t, state, "alpha", func(run pluginRunState) bool { return run.ReasonCode == "lease_held" })
	time.Sleep(2*supervisePoll + time.Second)
	if now := readRunState(t, state, "alpha"); now.State != runStopped || len(now.Restarts) != 0 || now.BackoffStep != 0 || held.State != runStopped {
		t.Fatalf("a held lease was charged: %s, %d failures, step %d", now.State, len(now.Restarts), now.BackoffStep)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	awaitRunState(t, state, "alpha", running)
}

// stubbornScript is the feed fixture plus an ordinary grandchild in the
// same process group that ignores TERM, HUP and INT and never reads stdin:
// only a group KILL removes it (or its own 120 s bound). Its PID is in grandchild.pid.
func stubbornScript() string {
	return strings.Replace(feedFixtureScript, "/bin/mkdir -p events || exit 2\n",
		"/bin/mkdir -p events || exit 2\n/bin/sh -c 'trap \"\" TERM HUP INT; echo $$ > grandchild.pid.tmp; /bin/mv grandchild.pid.tmp grandchild.pid; i=0; while [ $i -lt 120 ]; do /bin/sleep 1; i=$((i+1)); done' </dev/null >/dev/null 2>&1 &\n", 1)
}

func grandchildPID(t *testing.T, state string) int {
	t.Helper()
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(state, "plugins", "alpha.data", "grandchild.pid"))
		if err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the fixture grandchild did not start")
	return 0
}

func awaitGone(pid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if hostProcessGone(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func stubbornState(t *testing.T, binary string) string {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	for _, args := range [][]string{{"add", "alpha", "--exec", crashableFixture(t, stubbornScript())}, {"configure", "alpha", "--scope", "all"}, {"grant", "alpha", "--status"}, {"enable", "alpha"}} {
		if output, err := pluginOwnerCLI(t, binary, store.Dir, args...); err != nil {
			t.Fatal(args, len(output), err)
		}
	}
	return store.Dir
}

// S14: when a host dies first (SIGKILL), the controller still ends its
// whole group: the grandchild that ignores TERM is gone too.
func TestPluginSupervisorCleansGroupAfterHostDeath(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := stubbornState(t, binary)
	startSupervisor(t, binary, state)
	awaitRunState(t, state, "alpha", running)
	grandchild := grandchildPID(t, state)
	crashHost(t, state, "alpha")
	if !awaitGone(grandchild) {
		t.Fatal("a grandchild outlived its dead host's group cleanup")
	}
}

// S14: when the controller itself is killed, each host sees lifeline EOF
// and, as its last action, ends its own group: no ordinary descendant
// outlives a dead controller.
func TestPluginSupervisorControllerDeathCleansGroups(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := stubbornState(t, binary)
	cmd, _ := startSupervisor(t, binary, state)
	run := awaitRunState(t, state, "alpha", running)
	grandchild := grandchildPID(t, state)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if !awaitGone(run.PID) || !awaitGone(run.PluginPID) || !awaitGone(grandchild) {
		t.Fatal("a host group outlived its dead controller")
	}
}

// S14: controller death is bounded even while the host is stuck on a held
// journal lock: lifeline EOF ends the whole group after its grace, without
// waiting for the host's Run to return.
func TestPluginSupervisorControllerDeathUnderJournalLock(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := stubbornState(t, binary)
	cmd, _ := startSupervisor(t, binary, state)
	run := awaitRunState(t, state, "alpha", running)
	grandchild := grandchildPID(t, state)
	store, err := bus.NewStore(state, adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = bus.WithJournalWrite(store, func(bus.JournalView) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if !awaitGone(run.PID) || !awaitGone(run.PluginPID) || !awaitGone(grandchild) {
		t.Fatal("a host group outlived its dead controller while the journal lock was held")
	}
}

// M6: the real pre-lease window. The first controller's host is blocked
// reading its record (journal lock held) and has not taken the lease when
// that controller is killed. A second controller must not start another
// host before the recovery grace ends; once the lock is free, the plugin
// runs again under the new controller.
func TestPluginSupervisorPriorStartingRunBlocksSecondHost(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := supervisorFixture(t, binary, "alpha")
	store, err := bus.NewStore(state, adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = bus.WithJournalWrite(store, func(bus.JournalView) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	first, _ := startSupervisor(t, binary, state)
	old := awaitRunState(t, state, "alpha", func(run pluginRunState) bool { return run.State == runStarting && run.PID > 0 })
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	startSupervisor(t, binary, state)
	// The second controller's first scan comes after one poll; its grace
	// runs recoveryGrace from there. Look inside that window.
	time.Sleep(supervisePoll + recoveryGrace/2)
	if now := readRunState(t, state, "alpha"); now.PID != old.PID || now.ControllerInstance != old.ControllerInstance {
		t.Fatalf("a second host started inside the recovery grace: pid %d (was %d)", now.PID, old.PID)
	}
	close(release)
	released = true
	awaitRunState(t, state, "alpha", func(run pluginRunState) bool { return running(run) && run.ControllerInstance != old.ControllerInstance })
}
