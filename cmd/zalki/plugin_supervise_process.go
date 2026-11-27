package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// hostFrame mirrors the private frame a supervised host writes on its
// control pipe (internal/plugins). It is metadata only.
type hostFrame struct {
	Installation string `json:"installation,omitempty"`
	Epoch        string `json:"epoch,omitempty"`
	Revision     string `json:"revision,omitempty"`
	Phase        string `json:"phase"`
	HostPID      int    `json:"host_pid"`
	PluginPID    int    `json:"plugin_pid,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// hostEvent is one frame, or the end of the control pipe (closed when the
// host exits; the plugin never holds it).
type hostEvent struct {
	name  string
	host  *hostProcess
	frame *hostFrame
}

// hostProcess is one supervised host in its own process group (PGID = its
// PID). The leader stays unreaped until the final group signal, so its
// number cannot be reused under us; Wait is called exactly once, after it.
type hostProcess struct {
	cmd      *exec.Cmd
	lifeline *os.File // write end; closing it is the host's stop request
	logs     *rotatingLog
	output   *os.File      // read end of the host's stdout/stderr pipe
	ended    chan struct{} // closed by the frame reader when the pipe ends
	stopped  bool          // the controller asked for this stop
}

// hostArgs are the literal arguments of one supervised host: the state is
// pinned, logs are off, and the control pipe is FD 3.
func hostArgs(state, home, name string) []string {
	return []string{"--state", state, "--config", filepath.Join(home, ".config", "zalki", "config.json"), "--log-level", "off", "plugins", "run", name, "--supervised-control-fd=3"}
}

// startHost starts binary as a supervised host for name. Its environment
// is HOME and the fixed PATH only. stdout and stderr go to the capped
// private log; frames go to events until the control pipe ends.
func startHost(binary, state, home, name string, logs *rotatingLog, events chan<- hostEvent) (*hostProcess, error) {
	lifelineRead, lifeline, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		closeAll(lifelineRead, lifeline)
		return nil, err
	}
	// The controller owns every stop (lifeline, then group signals), so no
	// context may kill the host on its own.
	cmd := exec.CommandContext(context.Background(), binary, hostArgs(state, home, name)...) //nolint:gosec // G204: the validated Zalki executable with literal arguments.
	cmd.Env = []string{"HOME=" + home, "PATH=" + home + "/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.Dir = "/"
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		closeAll(lifelineRead, lifeline, controlRead, controlWrite)
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = lifelineRead, outputWrite, outputWrite
	cmd.ExtraFiles = []*os.File{controlWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// An escaped descendant holding the log pipe cannot hold up Wait.
	cmd.WaitDelay = time.Second
	err = cmd.Start()
	closeAll(lifelineRead, controlWrite, outputWrite)
	if err != nil {
		closeAll(lifeline, controlRead, outputRead)
		return nil, err
	}
	h := &hostProcess{cmd: cmd, lifeline: lifeline, logs: logs, output: outputRead, ended: make(chan struct{})}
	go logs.copyFrom(outputRead)
	go readFrames(name, h, controlRead, events)
	return h, nil
}

func closeAll(files ...*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}

// readFrames forwards bounded frames; a malformed or oversized frame ends
// the pipe like EOF. The final nil-frame event marks the end.
func readFrames(name string, h *hostProcess, control *os.File, events chan<- hostEvent) {
	defer control.Close() //nolint:errcheck // Read end only.
	reader := bufio.NewReaderSize(io.LimitReader(control, 1<<20), 512)
	for {
		line, err := reader.ReadSlice('\n')
		var frame hostFrame
		if err != nil || json.Unmarshal(line, &frame) != nil {
			close(h.ended)
			events <- hostEvent{name: name, host: h}
			return
		}
		events <- hostEvent{name: name, host: h, frame: &frame}
	}
}

// groupGrace bounds each stage of a group stop; with the final kill, one
// group stop stays within seven seconds. It is longer than a plugin's own
// 2 s EOF grace in its host, so a slow but correct plugin ends before the
// TERM.
const groupGrace = 3 * time.Second

// stop ends the host and its whole group: lifeline EOF first (orderly
// cancel), then TERM and KILL to the group while the leader is still
// unreaped, and only then Wait. It returns the host's exit status.
func (h *hostProcess) stop() *os.ProcessState {
	_ = h.lifeline.Close()
	pgid := h.cmd.Process.Pid
	if !waitFor(h.ended, groupGrace) {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		waitGroupGone(pgid, groupGrace)
	}
	// The final group signal: also removes ordinary descendants that
	// ignored EOF, even when the leader exited first.
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = h.cmd.Process.Kill()
	}
	_ = h.cmd.Wait()
	// An escaped descendant may still hold the write end: closing ours ends
	// the copy, so it never outlives this host.
	_ = h.output.Close()
	_ = h.logs.close()
	return h.cmd.ProcessState
}

func waitFor(ended <-chan struct{}, limit time.Duration) bool {
	select {
	case <-ended:
		return true
	case <-time.After(limit):
		return false
	}
}

// waitGroupGone polls until no process is left in the group or the limit
// passes. The unreaped leader keeps the group number ours meanwhile. On
// Darwin a group whose only member is the zombie leader answers EPERM, not
// ESRCH; the bounded zombie-only proof ends the wait then.
func waitGroupGone(pgid int, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		err := syscall.Kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) && groupOnlyZombies(pgid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// notifyExecutable runs the banner script. Tests always replace it with an
// owned fixture; banners are sent only for the default state.
var notifyExecutable = "/usr/bin/osascript"

// notifySource is the fixed AppleScript. The title and the text reach it
// only as argv items, never as script text.
var notifySource = [...]string{"on run argv", "display notification (item 2 of argv) with title (item 1 of argv)", "end run"}

// notifyArgs are the literal arguments of one banner child.
func notifyArgs(text string) []string {
	return []string{"-e", notifySource[0], "-e", notifySource[1], "-e", notifySource[2], notifyTitle, text}
}

// Bounds of one banner child: run time and output.
const (
	notifyRunLimit    = 2 * time.Second
	notifyOutputLimit = 4096
)

// notifyKill signals the child's group; tests observe it.
var notifyKill = syscall.Kill

// notifyOutcome: ok is a clean exit 0 within both bounds; cleaned is
// positive evidence that no member of the child's group is left running.
type notifyOutcome struct{ ok, cleaned bool }

// runNotifyChild runs one banner child in its own process group. The 2 s
// watchdog starts right after Start, before started runs (it saves the
// started phase and may be slow). The exit is seen without reaping (the
// unreaped leader keeps the group number ours); output EOF is only output.
// Then: the final group signal, an unconditional kill of the original
// handle, and exactly one Wait.
func runNotifyChild(text string, started func(pid int)) notifyOutcome {
	cmd := exec.CommandContext(context.Background(), notifyExecutable, notifyArgs(text)...) //nolint:gosec // G204: a fixed executable with literal arguments.
	cmd.Env, cmd.Dir = []string{"PATH=/usr/bin:/bin"}, "/"
	read, write, err := os.Pipe()
	if err != nil {
		return notifyOutcome{cleaned: true}
	}
	cmd.Stdout, cmd.Stderr = write, write
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	_ = write.Close()
	if err != nil {
		_ = read.Close()
		return notifyOutcome{cleaned: true} // nothing was started
	}
	watchdog := time.NewTimer(notifyRunLimit)
	defer watchdog.Stop()
	pgid := cmd.Process.Pid
	saved := make(chan struct{})
	go func() {
		defer close(saved)
		started(pgid)
	}()
	drained := make(chan int64, 1) // buffered: the reader never blocks on exit
	go func() {
		n, _ := io.Copy(io.Discard, io.LimitReader(read, notifyOutputLimit+1))
		drained <- n
	}()
	clean := awaitNotifyChild(pgid, watchdog.C, drained)
	cleaned := endNotifyGroup(pgid)
	_ = notifyKillLeader(cmd.Process) // the original handle, whatever the group answered
	_ = read.Close()
	// One Wait, in its own goroutine (its only caller), joined within a bound:
	// a leader that did not die is never waited for without end. Not joined
	// means not cleaned, so the reservation stays and dispatch is fenced.
	type reap struct {
		state *os.ProcessState
		err   error
	}
	joined, wait := make(chan reap, 1), notifyWait // read the seam once, here
	go func() {
		state, err := wait(cmd)
		joined <- reap{state, err}
	}()
	var done reap
	select {
	case done = <-joined:
	case <-time.After(notifyJoinLimit):
		return notifyOutcome{}
	}
	if done.state == nil {
		// Wait failed without a reap: the original is not known to be gone.
		return notifyOutcome{}
	}
	// The started save (a fingerprint read and a file write) is joined
	// within a bound too; not joined means unknown, and the dispatch fences.
	select {
	case <-saved:
	case <-time.After(notifyJoinLimit):
		return notifyOutcome{}
	}
	// A reaped non-zero exit is cleaned but failed.
	return notifyOutcome{ok: clean && done.err == nil && done.state.Success(), cleaned: cleaned}
}

// notifyJoinLimit bounds the one Wait after the final signals.
const notifyJoinLimit = 2 * time.Second

// notifyKillLeader kills the original child handle; tests make it fail.
var notifyKillLeader = (*os.Process).Kill

// notifyWait is the one Wait of a banner child. It returns the reap state
// (nil when Wait failed before a reap); tests observe or replace it.
var notifyWait = func(cmd *exec.Cmd) (*os.ProcessState, error) {
	err := cmd.Wait()
	return cmd.ProcessState, err
}

// awaitNotifyChild waits for both the leader's exit (seen as a zombie, not
// reaped) and the end of its output, within the watchdog. Too much output
// ends the wait at once.
func awaitNotifyChild(pid int, watchdog <-chan time.Time, drained <-chan int64) bool {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	exited, output := false, false
	for !exited || !output {
		select {
		case <-watchdog:
			return false
		case n := <-drained:
			if n > notifyOutputLimit {
				return false
			}
			output = true
		case <-tick.C:
			exited = exited || leaderExited(pid)
		}
	}
	return true
}

// endNotifyGroup sends the final group SIGKILL while the leader is unreaped
// and reports positive evidence that nothing of the group is left: the
// signal was delivered, no such group exists, or (Darwin's EPERM) every
// member is a zombie, read for a short time while the group is still leaving
// (awaitGroupOnlyZombies). Any other answer is unknown.
func endNotifyGroup(pgid int) bool {
	err := notifyKill(-pgid, syscall.SIGKILL)
	switch {
	case err == nil, errors.Is(err, syscall.ESRCH):
		return true
	case errors.Is(err, syscall.EPERM):
		return awaitGroupOnlyZombies(pgid)
	}
	return false
}
