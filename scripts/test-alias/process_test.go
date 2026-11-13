package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

func TestAliasChildGroupSignalBeforeReap(t *testing.T) {
	previous := signalAliasGroup
	priorObserve := observeAliasProcess
	var expected procwatch.Identity
	observeAliasProcess = func(ctx context.Context, pid int) (aliasChildRef, error) {
		ref, err := priorObserve(ctx, pid)
		expected = ref.identity
		return ref, err
	}
	unsafe := false
	calls := 0
	signalAliasGroup = func(group int, signal syscall.Signal) error {
		info, err := procwatch.Read(-group)
		if err != nil || info.PPID != os.Getpid() || -group != expected.PID || info.Start != expected.Start {
			unsafe = true
			return syscall.ESRCH
		}
		calls++
		return previous(group, signal)
	}
	defer func() { signalAliasGroup = previous; observeAliasProcess = priorObserve }()
	// Any descendant left by the fault is self-bounded; the hook above never
	// performs a numeric signal after authority is lost.
	err := runChild(t.Context(), t.TempDir(), os.Environ(), io.Discard, io.Discard, "/bin/sh", "-c", "sleep 0.1 & exit 0")
	if unsafe || calls != 1 {
		t.Fatal("ALIAS_GROUP_SIGNAL_AFTER_REAP")
	}
	if err != nil || calls != 1 {
		t.Fatal("owned leader was not signaled once before reap", calls, err)
	}
}

func TestAliasObservationFailureNeverSignalsGroup(t *testing.T) {
	priorObserve, priorSignal := observeAliasProcess, signalAliasGroup
	observeAliasProcess = func(context.Context, int) (aliasChildRef, error) {
		return aliasChildRef{}, errors.New("controlled observation unavailable")
	}
	calls := 0
	signalAliasGroup = func(int, syscall.Signal) error { calls++; return nil }
	defer func() { observeAliasProcess, signalAliasGroup = priorObserve, priorSignal }()
	err := runChild(t.Context(), t.TempDir(), os.Environ(), io.Discard, io.Discard, "/bin/sh", "-c", "sleep 0.1")
	if calls != 0 {
		t.Fatal("ALIAS_OBSERVATION_ERROR_SIGNALED_GROUP")
	}
	if err == nil {
		t.Fatal("observation failure became success")
	}
}

func TestAliasChildCancellationStopsOwnedGroup(t *testing.T) {
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	survived := filepath.Join(root, "survived")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runChild(ctx, root, os.Environ(), &output, &output, "/bin/sh", "-c", `(sleep 2; echo survived > "$2") & echo ready > "$1"; wait`, "fixture", ready, survived)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("bounded controlled child cleanup did not finish")
		}
	})
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("controlled child never became ready")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation reported success")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("owned child group was not stopped and reaped")
	}
	if _, err := os.Stat(survived); !os.IsNotExist(err) {
		t.Fatal("owned descendant survived cancellation", err)
	}
}

func TestAliasEarlyExitNotificationHasBoundedCleanup(t *testing.T) {
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sh", "-c", "sleep 0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	info, err := procwatch.Read(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	ref := aliasChildRef{identity: procwatch.Identity{PID: info.PID, Start: info.Start}, parent: info.PPID, uid: info.UID}
	priorRead, priorGroup, priorSignal := readAliasProcess, aliasProcessGroup, signalAliasGroup
	readAliasProcess = func(int) (procwatch.Info, error) { snapshot := info; snapshot.Zombie = false; return snapshot, nil }
	aliasProcessGroup = func(int) (int, error) { return ref.identity.PID, nil }
	calls := 0
	signalAliasGroup = func(int, syscall.Signal) error { calls++; return nil }
	defer func() { readAliasProcess, aliasProcessGroup, signalAliasGroup = priorRead, priorGroup, priorSignal }()
	start := time.Now()
	lost, observed := pollAliasExit(t.Context(), ref, 50*time.Millisecond)
	result := finishAliasChild(cmd, lost, observed)
	if lost.valid(cmd.Process.Pid) || calls != 0 || time.Since(start) > time.Second || result == nil || !strings.Contains(result.Error(), "child_exit_observation_failed") {
		t.Fatal("ALIAS_EARLY_EXIT_POLL_WAS_UNBOUNDED_OR_SIGNALED", calls, result)
	}
	if cmd.ProcessState == nil {
		t.Fatal("direct child cleanup did not reap")
	}
}

func TestAliasEarlyExitCancellationStopsGroup(t *testing.T) {
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	survived := filepath.Join(root, "survived")
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sh", "-c", `(sleep 0.5; echo survived > "$2") & echo ready > "$1"; wait`, "fixture", ready, survived)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controlled child never ready")
		}
		time.Sleep(time.Millisecond)
	}
	info, err := procwatch.Read(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	ref := aliasChildRef{identity: procwatch.Identity{PID: info.PID, Start: info.Start}, parent: info.PPID, uid: info.UID}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(25*time.Millisecond, cancel)
	defer timer.Stop()
	current, observed := pollAliasExit(ctx, ref, time.Second)
	result := finishAliasChild(cmd, current, observed)
	waited = true
	time.Sleep(600 * time.Millisecond)
	_, markerErr := os.Stat(survived)
	if !current.valid(ref.identity.PID) || result == nil || !errors.Is(result, context.Canceled) || !os.IsNotExist(markerErr) {
		t.Fatal("ALIAS_EARLY_EXIT_CANCELLATION_LEFT_GROUP", result, markerErr)
	}
}

func TestAliasFailedGroupSignalStillReapsChild(t *testing.T) {
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sleep", "2")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	info, err := procwatch.Read(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	ref := aliasChildRef{identity: procwatch.Identity{PID: info.PID, Start: info.Start}, parent: info.PPID, uid: info.UID}
	previous := signalAliasGroup
	calls := 0
	signalAliasGroup = func(int, syscall.Signal) error { calls++; return syscall.EIO }
	defer func() { signalAliasGroup = previous }()
	start := time.Now()
	result := finishAliasChild(cmd, ref, context.Canceled)
	if time.Since(start) > time.Second || cmd.ProcessState == nil || !errors.Is(result, syscall.EIO) || calls != 1 {
		t.Fatal("ALIAS_FAILED_GROUP_SIGNAL_DID_NOT_REAP_PROMPTLY", calls, result)
	}
}
