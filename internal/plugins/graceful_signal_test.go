package plugins

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

type signalFixtureHost struct {
	cmd                  *exec.Cmd
	done                 chan struct{}
	waitErr              error
	feed                 bus.PluginFeed
	data, stdout, stderr string
}

func signalFixture(t *testing.T) (bus.PluginFeed, string, string) {
	t.Helper()
	store, session := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	feed := bus.NewPluginFeed(store, "signal-child")
	data := filepath.Join(store.Dir, "plugins", "signal-child.data")
	if err := feed.AddExternalPlugin(binary, []string{ChildCommand, "--outbox", filepath.Join(data, "outbox")}); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginFeed([]string{session.ID}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, ".child-mode"), []byte("cleanup-eof"), 0600); err != nil {
		t.Fatal(err)
	}
	return feed, store.Dir, data
}

func startSignalFixtureHost(t *testing.T) *signalFixtureHost {
	t.Helper()
	feed, state, data := signalFixture(t)
	logs := t.TempDir()
	h := &signalFixtureHost{done: make(chan struct{}), feed: feed, data: data, stdout: filepath.Join(logs, "stdout"), stderr: filepath.Join(logs, "stderr")}
	output, err := os.Create(h.stdout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	diagnostic, err := os.Create(h.stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = diagnostic.Close() })
	h.cmd = exec.CommandContext(context.WithoutCancel(t.Context()), referenceBinary(t), "--state", state, "--log-level", "off", "plugins", "run", "signal-child")
	h.cmd.Env = []string{"HOME=" + notifierTemp(t)}
	// Only this fixture-owned host and its descendants enter this new session.
	h.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	h.cmd.Stdout, h.cmd.Stderr = output, diagnostic
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { h.waitErr = h.cmd.Wait(); close(h.done) }()
	t.Cleanup(func() {
		select {
		case <-h.done:
		default:
			_ = h.cmd.Process.Kill()
			select {
			case <-h.done:
			case <-time.After(5 * time.Second):
				t.Error("fixture host did not exit")
			}
		}
	})
	eventually(t, func() bool {
		select {
		case <-h.done:
			raw, _ := os.ReadFile(h.stderr)
			t.Fatal("signal fixture exited before ready", h.waitErr, string(raw))
		default:
		}
		raw, _ := os.ReadFile(h.stdout)
		return strings.Contains(string(raw), "signal-child ready\n")
	})
	return h
}

func (h *signalFixtureHost) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-h.done:
		return h.waitErr
	case <-time.After(5 * time.Second):
		t.Fatal("signal fixture host did not reap")
	}
	return nil
}

func (h *signalFixtureHost) requireCleanupAndReleasedLease(t *testing.T) {
	t.Helper()
	eventually(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(h.data, ".eof-cleaned"))
		return err == nil && string(raw) == "clean"
	})
	eventually(t, func() bool {
		lease, err := h.feed.AcquirePluginLease()
		if err != nil {
			return false
		}
		_ = lease.Close()
		return true
	})
}

func TestPluginForegroundGroupSignalsAllowEOFCleanup(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			h := startSignalFixtureHost(t)
			group, err := syscall.Getpgid(h.cmd.Process.Pid)
			if err != nil || group != h.cmd.Process.Pid || group == syscall.Getpgrp() {
				t.Fatal("refusing to signal a group not isolated by this fixture", group, err)
			}
			started := time.Now()
			// This is the private HOST group, never the child's isolated group.
			if err := syscall.Kill(-group, sig); err != nil {
				t.Fatal(err)
			}
			if err := h.wait(t); err != nil {
				raw, _ := os.ReadFile(h.stderr)
				t.Fatal("foreground group signal preempted EOF cleanup", err, string(raw))
			}
			if elapsed := time.Since(started); elapsed >= 2*time.Second {
				t.Fatal("child exit0 was not observed before the forced-kill grace deadline", elapsed)
			}
			h.requireCleanupAndReleasedLease(t)
		})
	}
}

func TestPluginHostSIGKILLStillClosesChildInputAndLease(t *testing.T) {
	h := startSignalFixtureHost(t)
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err := h.wait(t)
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal("fixture host did not report owned SIGKILL", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("fixture host had unexpected exit", err)
	}
	// The dead host cannot call stop: OS closes its pipe; the child's raw FD3
	// keeps the lease until the child finishes EOF cleanup and actually exits.
	h.requireCleanupAndReleasedLease(t)
}
