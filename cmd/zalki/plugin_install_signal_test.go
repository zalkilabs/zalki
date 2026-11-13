//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"github.com/zalkilabs/zalki/internal/plugins"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// cancelSignalCases are the cancel signals that only come from outside the
// terminal; SIGINT is covered by the real-terminal Ctrl-C tests.
var cancelSignalCases = []struct {
	name   string
	signal syscall.Signal
}{{"SIGTERM", syscall.SIGTERM}, {"SIGHUP", syscall.SIGHUP}}

// signalInstall is the real binary running plugins install --yes on pipes,
// with no terminal. Output is collected like a pty session.
type signalInstall struct {
	*ptySession
	process *os.Process
	state   *os.ProcessState
	exited  chan struct{}
}

// startSignalInstall starts the real binary with --yes for one session.
// Cleanup always kills zalki (its pid only) and waits for it.
func startSignalInstall(t *testing.T, binary, state, folder string) *signalInstall {
	t.Helper()
	args := []string{"--state", state, "--log-level", "off", "plugins", "install", "--yes", "--allow-unsandboxed", "--tree-sha256"}
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal("cannot hash signal fixture", err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	cmd := exec.CommandContext(t.Context(), binary, append(args, snapshot.TreeSHA256, folder)...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	cmd.Stdout, cmd.Stderr = write, write
	err = cmd.Start()
	_ = write.Close()
	if err != nil {
		t.Fatal(err)
	}
	run := &signalInstall{ptySession: &ptySession{master: read, more: make(chan struct{}, 1)}, process: cmd.Process, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		run.state = cmd.ProcessState
		close(run.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // zalki's pid only, never a group
		select {
		case <-run.exited:
		case <-time.After(10 * time.Second):
			t.Error("zalki was not reaped")
		}
	})
	go run.collect()
	return run
}

func (r *signalInstall) send(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := r.process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// waitExit waits for zalki to end; the result is valid after it returns.
func (r *signalInstall) waitExit(t *testing.T) *os.ProcessState {
	t.Helper()
	select {
	case <-r.exited:
		return r.state
	case <-time.After(5 * time.Second):
		t.Fatalf("zalki did not exit:\n%s", r.output())
		return nil
	}
}

// SIGTERM and SIGHUP cancel the install like Ctrl-C: during the build, zalki
// stops it, says so, exits 0 and installs nothing.
func TestPluginInstallSignalCancels(t *testing.T) {
	binary := checkRealBinary(t)
	for _, c := range cancelSignalCases {
		fds := openFDs(t)
		t.Run(c.name, func(t *testing.T) {
			store, _ := pluginOwnerFixture(t)
			build := newGatedBuild(t, 200, "0") // never released: the cancel must stop it
			run := startSignalInstall(t, binary, store.Dir, build.folder)
			deadline := time.After(20 * time.Second)
			run.waitFor(t, "Building...", deadline)
			build.pid(t) // the build runs: the signal must stop it, not prevent it
			run.send(t, c.signal)
			run.waitFor(t, "Cancelled. Nothing was installed.", deadline)
			if state := run.waitExit(t); state.ExitCode() != 0 {
				t.Fatalf("exit %v after %s, want 0:\n%s", state, c.name, run.output())
			}
			if strings.Contains(run.output(), "build failed") {
				t.Fatal(run.output())
			}
			assertNothingInstalled(t, store.Dir)
		})
		if after := openFDs(t); after != fds {
			t.Fatalf("%s: %d open files after the test, %d before", c.name, after, fds)
		}
	}
}

// At the point of no return the first SIGTERM or SIGHUP cannot stop the
// install; the second one ends the process at once by its default action.
func TestPluginInstallSecondSignalKills(t *testing.T) {
	binary := checkRealBinary(t)
	for _, c := range cancelSignalCases {
		fds := openFDs(t)
		t.Run(c.name, func(t *testing.T) {
			store, _ := pluginOwnerFixture(t)
			// The build waits at the gate, so the test can take the Store lock
			// after zalki has read its sessions and before it commits.
			build := newGatedBuild(t, 200, "0")
			run := startSignalInstall(t, binary, store.Dir, build.folder)
			deadline := time.After(20 * time.Second)
			run.waitFor(t, "Building...", deadline)
			lock := holdStoreLock(t, store.Dir)
			defer lock.Close()
			build.release(t)
			run.waitFor(t, "Enabling...", deadline)
			waitForPin(t, store.Dir, "demo", deadline)
			run.send(t, c.signal)
			select {
			case <-run.exited:
				t.Fatalf("the first %s ended the commit wait:\n%s", c.name, run.output())
			case <-time.After(stillWaitingWindow):
			}
			run.send(t, c.signal)
			state := run.waitExit(t)
			if status, ok := state.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != c.signal {
				t.Fatalf("exit %v after the second %s, want killed by it:\n%s", state, c.name, run.output())
			}
			if _, err := os.Stat(filepath.Join(store.Dir, "plugins", "demo.json")); err == nil {
				t.Fatal("a record was written although the lock was held")
			}
		})
		if after := openFDs(t); after != fds {
			t.Fatalf("%s: %d open files after the test, %d before", c.name, after, fds)
		}
	}
}

// gatedBuild is a fake go build that waits for a gate file. It polls in a
// bounded loop (steps of 0.1 s), so it ends by that bound at the latest, even
// when zalki is killed and cannot stop it. It marks that it started, then
// (after pidDelay) writes its pid, which is also its process group:
// RunInstallBuild starts it with Setpgid. Cleanup writes a private stop marker and waits
// until it is gone, if the pid appears within stopPIDWait.
type gatedBuild struct{ folder, gate, started, pidFile, stopFile string }

// stopPIDWait is how long stop waits for a started build's pid file.
const stopPIDWait = 2 * time.Second

func newGatedBuild(t *testing.T, steps int, pidDelay string) *gatedBuild {
	t.Helper()
	dir := t.TempDir()
	b := &gatedBuild{gate: filepath.Join(dir, "gate"), started: filepath.Join(dir, "started"), pidFile: filepath.Join(dir, "pid"), stopFile: filepath.Join(dir, "stop")}
	b.folder = fakeGoFolder(t, fmt.Sprintf(`: > '%[4]s'; sleep %[5]s
echo $$ > '%[1]s.new' && mv '%[1]s.new' '%[1]s'
i=0
while [ ! -e '%[2]s' ]; do [ -e '%[6]s' ] && exit 0; i=$((i+1)); [ $i -gt %[3]d ] && exit 3; sleep 0.1; done
printf built > plugin; chmod 700 plugin`, b.pidFile, b.gate, steps, b.started, pidDelay, b.stopFile))
	t.Cleanup(func() { b.stop(t) })
	return b
}

func (b *gatedBuild) release(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(b.gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

// readPID polls for the pid file for up to wait; ok is false if it is absent.
func (b *gatedBuild) readPID(t *testing.T, wait time.Duration) (int, bool) {
	t.Helper()
	for end := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		if data, err := os.ReadFile(b.pidFile); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			return pid, true
		}
		if time.Now().After(end) {
			return 0, false
		}
	}
}

// pid waits until the build has published its pid and returns it.
func (b *gatedBuild) pid(t *testing.T) int {
	t.Helper()
	pid, ok := b.readPID(t, 10*time.Second)
	if !ok {
		t.Fatal("the fake build did not publish its pid")
	}
	return pid
}

// stop asks the fixture to exit through its private marker, then observes its
// recorded PID without signalling it. Without a pid by then, it never started
// or starts later; such a build still ends by its loop bound at the latest.
func (b *gatedBuild) stop(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(b.stopFile, nil, 0600); err != nil {
		t.Error(err)
		return
	}
	pid, ok := b.readPID(t, stopPIDWait)
	if !ok {
		return
	}
	if !processGone(pid, 5*time.Second) {
		t.Errorf("the fake build %d is still running", pid)
	}
}

// processGone reports whether pid is gone (ESRCH) within the wait.
func processGone(pid int, wait time.Duration) bool {
	for end := time.Now().Add(wait); ; time.Sleep(50 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// When zalki is killed before the build reaches its gate, zalki cannot stop
// the build. The fake build then ends by its own loop bound at the latest, and
// at once when cleanup writes its stop marker within stopPIDWait, even a pid published
// after zalki died.
func TestPluginInstallFakeBuildEndsAfterZalkiIsKilled(t *testing.T) {
	binary := checkRealBinary(t)
	start := func(t *testing.T, steps int, pidDelay string) *gatedBuild {
		t.Helper()
		return killZalkiAfterBuildStarts(t, binary, steps, pidDelay)
	}
	t.Run("the build ends by its own bound at the latest", func(t *testing.T) {
		pid := start(t, 10, "0").pid(t) // bound 1 s
		if processGone(pid, 0) {
			t.Fatal("the fake build ended with zalki; this test needs it to outlive zalki")
		}
		if !processGone(pid, 5*time.Second) {
			t.Fatal("the fake build did not end by its loop bound")
		}
	})
	t.Run("cleanup stops a build whose pid it sees", func(t *testing.T) {
		build := start(t, 300, "0") // bound 30 s, longer than the checks below
		pid := build.pid(t)
		if processGone(pid, 0) {
			t.Fatal("the fake build ended with zalki; this test needs it to outlive zalki")
		}
		build.stop(t)
		if !processGone(pid, 0) {
			t.Fatal("the fake build is still running after stop")
		}
	})
	t.Run("cleanup waits for a pid published after zalki died", func(t *testing.T) {
		build := start(t, 300, "1") // the pid appears 1 s after the start mark
		if _, err := os.Stat(build.pidFile); err == nil {
			t.Fatal("the pid was published before stop; this test needs it later")
		}
		build.stop(t)
		pid, ok := build.readPID(t, 0)
		if !ok {
			t.Fatal("stop returned before the late pid was published")
		}
		if !processGone(pid, 0) {
			t.Fatal("the fake build with a late pid is still running after stop")
		}
	})
}

// killZalkiAfterBuildStarts runs an install with a gated build and kills zalki
// (its pid only, never the build's group) once the build has started.
func killZalkiAfterBuildStarts(t *testing.T, binary string, steps int, pidDelay string) *gatedBuild {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	build := newGatedBuild(t, steps, pidDelay)
	run := startSignalInstall(t, binary, store.Dir, build.folder)
	run.waitFor(t, "Building...", time.After(20*time.Second))
	build.waitStarted(t)
	if err := run.process.Kill(); err != nil {
		t.Fatal(err)
	}
	run.waitExit(t)
	return build
}

// waitStarted waits for the build's start mark.
func (b *gatedBuild) waitStarted(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(b.started); err == nil {
			return
		}
		if time.Now().After(end) {
			t.Fatal("the fake build did not start")
		}
	}
}

// openFDs counts this process's open files, so a test can check that its
// pipes and lock were all closed by cleanup.
func openFDs(t *testing.T) int {
	t.Helper()
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1) // names only: a stat of each would fail on dir's own fd
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}
