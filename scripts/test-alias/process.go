package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

var signalAliasGroup = syscall.Kill
var observeAliasProcess = observeAliasChild
var readAliasProcess = procwatch.Read
var aliasProcessGroup = readAliasProcessGroup

// Observation never reaps. The privately owned leader reserves its PID/PGID
// through the last group signal; only then may Wait reap and drain the pipes.
func runChild(ctx context.Context, dir string, env []string, out, diagnostic io.Writer, args ...string) error {
	if _, err := fmt.Fprintf(diagnostic, "Command: %q\n", args); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), args[0], args[1:]...) //nolint:gosec // G204: literal gate argv; cancellation is ordered before reaping below.
	cmd.Cancel = nil                                                             // No asynchronous signal may race Process.Wait or pipe drain.
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, out, diagnostic
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	ref, observed := observeAliasProcess(ctx, cmd.Process.Pid)
	return finishAliasChild(cmd, ref, observed)
}

type aliasChildRef struct {
	identity procwatch.Identity
	parent   int
	uid      uint32
}

func (ref aliasChildRef) valid(pid int) bool {
	return ref.identity.PID == pid && ref.identity.Start != "" && ref.parent == os.Getpid() && int(ref.uid) == os.Getuid()
}

func finishAliasChild(cmd *exec.Cmd, ref aliasChildRef, observed error) error {
	signalErr := stopAliasChild(cmd.Process, ref)
	directErr := killAliasChild(cmd.Process)
	waitErr := cmd.Wait()
	return errors.Join(aliasStageError("observe child", observed), aliasStageError("signal child", signalErr), aliasStageError("kill child", directErr), aliasStageError("wait child", waitErr))
}

func stopAliasChild(process *os.Process, ref aliasChildRef) error {
	var err error
	if ref.valid(process.Pid) {
		if _, verifyErr := currentAliasChild(ref); verifyErr != nil {
			return verifyErr
		}
		err = signalAliasGroup(-process.Pid, syscall.SIGKILL)
	}
	if ref.valid(process.Pid) && errors.Is(err, syscall.EPERM) && aliasGroupExited(ref) {
		return nil
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func observeAliasChild(ctx context.Context, pid int) (ref aliasChildRef, err error) {
	info, err := readAliasProcess(pid)
	if err != nil {
		return ref, err
	}
	ref = aliasChildRef{identity: procwatch.Identity{PID: pid, Start: info.Start}, parent: info.PPID, uid: info.UID}
	if !ref.valid(pid) {
		return aliasChildRef{}, errors.New("child identity unavailable")
	}
	if info.Zombie {
		return ref, nil
	}
	source, err := procwatch.NewSource()
	if err != nil {
		return pollAliasChild(ctx, ref)
	}
	defer func() {
		if closeErr := source.Close(); closeErr != nil {
			ref = aliasChildRef{}
			err = errors.Join(err, closeErr)
		}
	}()
	if err := source.WatchProcess(ref.identity); err != nil {
		return pollAliasChild(ctx, ref)
	}
	for {
		select {
		case <-ctx.Done():
			return ref, ctx.Err()
		case event := <-source.Events():
			switch event.Kind {
			case procwatch.ProcessExited:
				if event.Process == ref.identity {
					return pollAliasExit(ctx, ref, 5*time.Second)
				}
			case procwatch.SourceFailed:
				return pollAliasChild(ctx, ref)
			case procwatch.DirectoryChanged:
			}
		}
	}
}

// Read observes but never reaps. A resolved owned incarnation may use the
// ordered group signal; unresolved read/identity errors may not.
func pollAliasChild(ctx context.Context, ref aliasChildRef) (aliasChildRef, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := currentAliasChild(ref)
		if err != nil {
			return aliasChildRef{}, err
		}
		if info.Zombie {
			return ref, nil
		}
		select {
		case <-ctx.Done():
			return ref, ctx.Err()
		case <-ticker.C:
		}
	}
}

func aliasStageError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", stage, err)
}

func currentAliasChild(ref aliasChildRef) (procwatch.Info, error) {
	info, err := readAliasProcess(ref.identity.PID)
	if err != nil {
		return info, err
	}
	group, err := aliasProcessGroup(ref.identity.PID)
	if err != nil {
		return info, err
	}
	if !ref.valid(info.PID) || info.PPID != ref.parent || info.UID != ref.uid || info.Start != ref.identity.Start || group != ref.identity.PID {
		return info, errors.New("child identity unavailable")
	}
	return info, nil
}

func pollAliasExit(ctx context.Context, ref aliasChildRef, limit time.Duration) (aliasChildRef, error) {
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	current, err := pollAliasChild(bounded, ref)
	if err != nil {
		if ctx.Err() != nil && current.valid(ref.identity.PID) {
			return current, fmt.Errorf("child_exit_observation_failed: %w", ctx.Err())
		}
		return aliasChildRef{}, fmt.Errorf("child_exit_observation_failed: %w", err)
	}
	return current, nil
}

func killAliasChild(process *os.Process) error {
	err := process.Kill()
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
