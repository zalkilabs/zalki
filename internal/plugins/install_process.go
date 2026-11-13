package plugins

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

var signalInstallBuildGroup = syscall.Kill
var observeInstallBuildProcess = observeInstallBuildChild
var readInstallBuildProcess = procwatch.Read
var installBuildProcessGroup = readInstallBuildProcessGroup

// The owner never starts Wait until observation and the final group signal are
// complete. The unreaped child reserves its PID; its own PGID is revalidated.
type installBuildChildRef struct {
	identity procwatch.Identity
	parent   int
	uid      uint32
}

func (ref installBuildChildRef) valid(pid int) bool {
	return ref.identity.PID == pid && ref.identity.Start != "" && ref.parent == os.Getpid() && int(ref.uid) == os.Getuid()
}

// runInstallBuildChild owns output draining without allowing exec.Wait to reap
// the leader first. A delayed burst still reaches the cap during this bound.
func runInstallBuildChild(ctx context.Context, cmd *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	read, write, err := os.Pipe()
	if err != nil {
		return err
	}
	output := cmd.Stdout
	cmd.Stdout, cmd.Stderr = write, write
	if err := cmd.Start(); err != nil {
		return errors.Join(err, read.Close(), write.Close())
	}
	closeErr := write.Close()
	drained := make(chan error, 1)
	go func() {
		_, err := io.Copy(output, read)
		if err != nil {
			cancel()
		}
		drained <- err
		close(drained)
	}()
	err = finishInstallBuildChild(ctx, cmd, drained)
	readErr := read.Close()
	copyErr := <-drained
	if errors.Is(copyErr, os.ErrClosed) {
		copyErr = nil
	}
	return errors.Join(err, closeErr, readErr, copyErr)
}

func awaitInstallBuildOutput(ctx context.Context, drained <-chan error) error {
	if drained == nil {
		return nil
	}
	timer := time.NewTimer(installWaitDelay)
	defer timer.Stop()
	select {
	case err := <-drained:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return exec.ErrWaitDelay
	}
}

func finishInstallBuildChild(ctx context.Context, cmd *exec.Cmd, drained <-chan error) error {
	ref, observed := observeInstallBuildProcess(ctx, cmd.Process.Pid)
	drainErr := awaitInstallBuildOutput(ctx, drained)
	signalErr := stopInstallBuildChild(cmd, ref)
	directErr := killInstallBuildChild(cmd.Process)
	waitErr := cmd.Wait()
	return errors.Join(installBuildStageError("observe child", observed), installBuildStageError("drain output", drainErr), installBuildStageError("signal child", signalErr), installBuildStageError("kill child", directErr), installBuildStageError("wait child", waitErr))
}

func stopInstallBuildChild(cmd *exec.Cmd, ref installBuildChildRef) error {
	// leader-unreaped: only the owner before its sole Wait may signal the group.
	if cmd.ProcessState != nil {
		return errors.New("build child already waited; group signal refused")
	}
	process := cmd.Process
	var err error
	if ref.valid(process.Pid) {
		if _, verifyErr := currentInstallBuildChild(ref); verifyErr != nil {
			return verifyErr
		}
		err = signalInstallBuildGroup(-process.Pid, syscall.SIGKILL)
	}
	if ref.valid(process.Pid) && errors.Is(err, syscall.EPERM) && installBuildGroupExited(ref) {
		return nil
	}
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func observeInstallBuildChild(ctx context.Context, pid int) (ref installBuildChildRef, err error) {
	info, err := readInstallBuildProcess(pid)
	if err != nil {
		return ref, err
	}
	ref = installBuildChildRef{identity: procwatch.Identity{PID: pid, Start: info.Start}, parent: info.PPID, uid: info.UID}
	if !ref.valid(pid) {
		return installBuildChildRef{}, errors.New("child identity unavailable")
	}
	if info.Zombie {
		return ref, nil
	}
	return pollInstallBuildChild(ctx, ref)
}

// Read observes but never reaps. A resolved owned incarnation may use the
// ordered group signal; unresolved read/identity errors may not.
func pollInstallBuildChild(ctx context.Context, ref installBuildChildRef) (installBuildChildRef, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := currentInstallBuildChild(ref)
		if err != nil {
			return installBuildChildRef{}, err
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

func installBuildStageError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", stage, err)
}

func currentInstallBuildChild(ref installBuildChildRef) (procwatch.Info, error) {
	info, err := readInstallBuildProcess(ref.identity.PID)
	if err != nil {
		return info, err
	}
	group, err := installBuildProcessGroup(ref.identity.PID)
	if err != nil {
		return info, err
	}
	if !ref.valid(info.PID) || info.PPID != ref.parent || info.UID != ref.uid || info.Start != ref.identity.Start || group != ref.identity.PID {
		return info, errors.New("child identity unavailable")
	}
	return info, nil
}

func killInstallBuildChild(process *os.Process) error {
	err := process.Kill()
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
