package plugins

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func gracefulChildFixture(t *testing.T, mode string) (*child, context.CancelFunc, pluginfeed.PluginFeed, *os.File, string) {
	t.Helper()
	store, _ := hostStore(t)
	data := hostData(store)
	if err := os.WriteFile(filepath.Join(data, ".child-mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	lease, err := feed.AcquirePluginLease()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	c, err := launchChild(ctx, binary, identityOf(t, binary), []string{"host-fake-child"}, data, "", lease)
	if err != nil {
		cancel()
		_ = lease.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = c.stop(nil); _ = lease.Close() })
	header, err := c.receive(2 * time.Second)
	if err != nil || !isHello(header) {
		t.Fatal("fixture hello", err)
	}
	return c, cancel, feed, lease, data
}

func TestPluginCancellationAllowsEOFCleanupAndZeroExit(t *testing.T) {
	c, cancel, _, _, data := gracefulChildFixture(t, "cleanup-eof")
	cancel()
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not deliver EOF")
	}
	if err := c.stop(nil); err != nil {
		t.Fatal("cooperative cleanup did not exit0", err)
	}
	if data, err := os.ReadFile(filepath.Join(data, ".eof-cleaned")); err != nil || string(data) != "clean" {
		t.Fatal("EOF cleanup did not run", err)
	}
	if c.waitErr != nil || !c.cmd.ProcessState.Success() || c.killedForCancel {
		t.Fatal("cooperative child was killed", c.waitErr)
	}
	if err := c.stop(nil); err != nil {
		t.Fatal("repeated stop changed outcome", err)
	}
}

func TestPluginCancellationKeepsLeaseThroughGraceAndKillsIgnoredEOF(t *testing.T) {
	c, cancel, feed, lease, data := gracefulChildFixture(t, "hold-eof")
	started := time.Now()
	cancel()
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(data, ".eof-seen")); return err == nil })
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := feed.AcquirePluginLease(); err == nil {
		_ = other.Close()
		t.Fatal("FD3 lease released before child OS exit")
	}
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("ignored EOF did not reach bounded kill/reap")
	}
	if err := c.stop(nil); err == nil {
		t.Fatal("uncooperative child was not killed")
	}
	if elapsed := time.Since(started); elapsed < 1500*time.Millisecond || elapsed > 6*time.Second {
		t.Fatal("EOF grace was preempted or unbounded", elapsed)
	}
	if !c.killedForCancel || !c.canceledStop(c.parent, nil) {
		t.Fatal("intentional grace kill lost provenance")
	}
	other, err := feed.AcquirePluginLease()
	if err != nil {
		t.Fatal("reaped child retained lease", err)
	}
	_ = other.Close()
}

func TestPluginCancellationPreservesGenuineNonzeroExit(t *testing.T) {
	c, cancel, _, _, _ := gracefulChildFixture(t, "error-eof")
	cancel()
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		t.Fatal("EOF error child did not exit")
	}
	err := c.stop(nil)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 17 {
		t.Fatal("genuine exit status lost", err)
	}
	if c.canceledStop(c.parent, errChildCanceled) {
		t.Fatal("cancellation concealed genuine nonzero exit")
	}
}

func TestPluginAlreadyCanceledContextDoesNotLaunch(t *testing.T) {
	store, _ := hostStore(t)
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	lease, err := feed.AcquirePluginLease()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c, err := launchChild(ctx, binary, identityOf(t, binary), []string{"host-fake-child"}, hostData(store), "", lease)
	if c != nil {
		_ = c.stop(nil)
		t.Fatal("already-canceled context launched a child")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation cause lost", err)
	}
}

func TestPluginCancellationDoesNotRelabelExternalKill(t *testing.T) {
	c, cancel, _, _, data := gracefulChildFixture(t, "hold-eof")
	cancel()
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(data, ".eof-seen")); return err == nil })
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := c.stop(nil); err == nil {
		t.Fatal("external kill was lost")
	}
	if c.killedForCancel || c.canceledStop(c.parent, nil) {
		t.Fatal("external kill was mislabeled as intentional grace kill")
	}
}

// These tests run serially. Inspect only plugin-owned goroutine frames, rather
// than a global goroutine count that includes unrelated runtime workers.
func pluginGoroutineContains(needles ...string) bool {
	stacks := make([]byte, 1<<20)
	count := runtime.Stack(stacks, true)
	for _, stack := range strings.Split(string(stacks[:count]), "\n\n") {
		found := true
		for _, needle := range needles {
			found = found && strings.Contains(stack, needle)
		}
		if found {
			return true
		}
	}
	return false
}

func TestPluginCancellationReentryLeavesNoShutdownGoroutines(t *testing.T) {
	store, _ := hostStore(t)
	cancel, done := startHost(t, store)
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	eventually(t, func() bool {
		status, err := feed.PluginStatus()
		return err == nil && !status.GapPending
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("cancel re-entry did not return cleanly", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel re-entry deadlocked")
	}
	eventually(t, func() bool {
		return !pluginGoroutineContains("plugins.(*child).stop") &&
			!pluginGoroutineContains("plugins.launchChild.func")
	})
	lease, err := feed.AcquirePluginLease()
	if err != nil {
		t.Fatal("returned Run retained child lease", err)
	}
	_ = lease.Close()
}

func TestPluginCancellationWhileHelloOrACKBlocked(t *testing.T) {
	for _, mode := range []string{"eof-before-hello", "no-hello", "no-ack"} {
		t.Run(mode, func(t *testing.T) { cancellationBlockedReceive(t, mode) })
	}
}

func cancellationBlockedReceive(t *testing.T, mode string) {
	t.Helper()
	store, _ := hostStore(t)
	data := hostData(store)
	if err := os.WriteFile(filepath.Join(data, ".child-mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	cancel, done := startHost(t, store)
	if mode == "no-ack" {
		// The child saw the offer and holds its ACK; the host waits for it.
		eventually(t, func() bool {
			matches, err := filepath.Glob(filepath.Join(data, ".seen-*"))
			return err == nil && len(matches) > 0
		})
	} else {
		eventually(t, func() bool {
			_, err := os.Stat(filepath.Join(data, ".child-env"))
			return err == nil && pluginGoroutineContains("plugins.(*child).receive", "internal/poll.(*FD).Read")
		})
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("owner cancellation while receive blocked was not clean", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receive cancellation did not reap")
	}
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	lease, err := feed.AcquirePluginLease()
	if err != nil {
		t.Fatal("receive cancellation retained FD3", err)
	}
	_ = lease.Close()
	if mode == "no-ack" {
		status, err := feed.PluginStatus()
		if err != nil || !status.GapPending {
			t.Fatal("cancellation acknowledged an unacknowledged offer", err)
		}
	}
}

func TestPluginCancellationUnblocksActualPipeWrite(t *testing.T) {
	c, cancel, _, _, _ := gracefulChildFixture(t, "no-read")
	// The child never reads stdin. Fill its real pipe until a deadline proves
	// backpressure; the subsequent exchange must block in the OS write path.
	if err := c.input.SetWriteDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.input.Write(make([]byte, 1<<20)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("fixture did not fill the child pipe", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.exchange(frame{ID: "cancel-write", Gap: "enabled"}) }()
	eventually(t, func() bool {
		return pluginGoroutineContains("plugins.(*child).exchange", "internal/poll.(*FD).Write")
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errChildCanceled) {
			t.Fatal("blocked write did not retain cancellation cause", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not unblock actual pipe write")
	}
	if err := c.stop(nil); err == nil || !c.canceledStop(c.parent, errChildCanceled) {
		t.Fatal("blocked write cancellation did not kill and reap intentionally", err)
	}
}

func TestPluginCancellationPreservesObservedPipeDeadline(t *testing.T) {
	c, cancel, _, _, _ := gracefulChildFixture(t, "cleanup-eof")
	_, cause := c.receive(25 * time.Millisecond)
	if !errors.Is(cause, os.ErrDeadlineExceeded) || errors.Is(cause, errChildCanceled) {
		t.Fatal("uncanceled pipe deadline was not preserved", cause)
	}
	cancel()
	if err := c.stop(cause); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("cancellation concealed previously observed pipe deadline", err)
	}
	if c.canceledStop(c.parent, cause) {
		t.Fatal("previously observed deadline was reclassified as cancellation")
	}
}

func TestPluginCancellationBeforePipeDeadlineSetup(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "active-owner"
		if canceled {
			name = "canceled-owner"
		}
		t.Run(name, func(t *testing.T) {
			c, cancel, _, _, _ := gracefulChildFixture(t, "cleanup-eof")
			if canceled {
				cancel()
			}
			if err := c.stop(nil); err != nil {
				t.Fatal("cooperative stop", err)
			}
			writeErr := c.exchange(frame{ID: "closed-setup", Gap: "enabled"})
			_, readErr := c.receive(time.Second)
			for _, err := range []error{writeErr, readErr} {
				if err == nil || errors.Is(err, errChildCanceled) != canceled {
					t.Errorf("closed deadline setup lost owner cancellation provenance (canceled=%v): %v", canceled, err)
				}
			}
		})
	}
}
