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
// group stop stays within five seconds.
const groupGrace = 2 * time.Second

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
// passes. The unreaped leader keeps the group number ours meanwhile.
func waitGroupGone(pgid int, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
