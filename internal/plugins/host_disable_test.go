package plugins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// ZAL-71: the host stops the child itself when the owner disables it, so
// any exit code of a child that leaves by itself is a quiet stop. A signal
// death, a kill after the grace, a stderr flood, a host failure and a stop
// for another reason stay errors.
func TestHostDisableEndsQuietlyWhenPluginExitsNonZero(t *testing.T) {
	t.Run("feed only", func(t *testing.T) { checkQuietDisable(t, false) })
	t.Run("receive", func(t *testing.T) { checkQuietDisable(t, true) }) // stderr withheld
	t.Run("signal death", func(t *testing.T) {
		checkLoudStop(t, "selfterm-on-code", true, "a plugin that died by a signal at disable was reported as clean")
	})
	t.Run("ignored stop", func(t *testing.T) {
		checkLoudStop(t, "ignore-on-code", false, "a plugin that ignored the stop was reported as clean")
	})
	t.Run("canceled during the grace", checkCancelAfterDisable)
	t.Run("stderr flood", checkFloodAtDisable)
	t.Run("host failure", func(t *testing.T) {
		// The host refuses the wrong ACK of its first offer; the child then
		// exits 0 on EOF.
		_, done := launchDisableHost(t, "bad-ack", false)
		if hostResult(t, done, 8*time.Second) == nil {
			t.Fatal("a host failure with a clean child exit was reported as success")
		}
	})
	t.Run("stale binding", checkStaleBindingNotQuiet)
}

func checkQuietDisable(t *testing.T, receive bool) {
	t.Helper()
	store, done := startDisableHost(t, "exit1-on-code", receive)
	disableHost(t, store)
	if err := hostResult(t, done, 5*time.Second); err != nil {
		t.Fatalf("a disable must end the host quietly even when the plugin exits 1: %v", err)
	}
	lease, err := pluginfeed.NewPluginFeed(store, "fake-notifier").AcquirePluginLease()
	if err != nil {
		t.Fatal("the lease is still held after Run returned", err)
	}
	_ = lease.Close()
}

// checkLoudStop: a disable whose child does not leave by itself with an
// exit status stays an error.
func checkLoudStop(t *testing.T, mode string, receive bool, failure string) {
	t.Helper()
	store, done := startDisableHost(t, mode, receive)
	disableHost(t, store)
	if hostResult(t, done, 8*time.Second) == nil {
		t.Fatal(failure)
	}
}

// checkCancelAfterDisable is the requested-stop boundary itself: a real child
// that holds EOF longer than the grace (hold-eof, 3 s), the run canceled
// before the stop, then finish with requested=true. The kill after the 2 s
// grace is a signal death and stays an error (ZAL-71, BUNDLE-M1).
func checkCancelAfterDisable(t *testing.T) {
	c, cancel, _, _, data := gracefulChildFixture(t, "hold-eof")
	cancel()
	err := c.finish(c.parent, nil, true)
	if _, statErr := os.Stat(filepath.Join(data, ".eof-seen")); statErr != nil {
		t.Fatal("the child never saw the stop", statErr)
	}
	if err == nil {
		t.Fatal("a forced kill after a disable was reported as clean when the run was canceled")
	}
}

func checkFloodAtDisable(t *testing.T) {
	store, done := startDisableHost(t, "loud-on-code", false)
	disableHost(t, store)
	if err := hostResult(t, done, 5*time.Second); err == nil || !strings.Contains(err.Error(), "plugin stderr exceeded 8KiB") {
		t.Fatalf("a stderr overflow at disable was hidden: %v", err)
	}
}

func checkStaleBindingNotQuiet(t *testing.T) {
	store, done := startDisableHost(t, "exit1-on-code", false)
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").ConfigurePluginScope("none"); err != nil {
		t.Fatal(err)
	}
	if hostResult(t, done, 5*time.Second) == nil {
		t.Fatal("a stale-binding stop was reported as a quiet disable")
	}
}

// startDisableHost starts the host with the child mode and returns after the
// hello and the first offer are done.
func startDisableHost(t *testing.T, mode string, receive bool) (bus.Store, <-chan error) {
	t.Helper()
	store, done := launchDisableHost(t, mode, receive)
	eventually(t, func() bool {
		s, err := pluginfeed.NewPluginFeed(store, "fake-notifier").PluginStatus()
		return err == nil && !s.GapPending
	})
	return store, done
}

func launchDisableHost(t *testing.T, mode string, receive bool) (bus.Store, <-chan error) {
	t.Helper()
	store, _ := hostStore(t)
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	if receive {
		if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true, Receive: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	_, done := startHost(t, store)
	return store, done
}

func disableHost(t *testing.T, store bus.Store) {
	t.Helper()
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
}

func hostResult(t *testing.T, done <-chan error, limit time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatal("the host did not end")
		return nil
	}
}
