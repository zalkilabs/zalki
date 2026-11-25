package plugins

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// firstOfferFailure waits up to limit until the host's first offer is
// acknowledged. It returns "" then, or text that says why it did not happen:
// the host already ended with an error, or it still runs and this is its last
// status. The text names no state path.
func firstOfferFailure(store bus.Store, done <-chan error, limit time.Duration) string {
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	var status pluginfeed.PluginFeedStatus
	var statusErr error
	if eventuallyWithin(limit, func() bool {
		status, statusErr = feed.PluginStatus()
		return statusErr == nil && !status.GapPending
	}) {
		return ""
	}
	select {
	case err := <-done:
		return "the host ended before its first offer was acknowledged: " + withoutStatePath(store, err)
	default:
		return fmt.Sprintf("the host still runs but its first offer was not acknowledged: last status error: %s; gap pending: %v; last gap: %q",
			withoutStatePath(store, statusErr), status.GapPending, status.LastGap)
	}
}

// waitFirstOffer fails the test with firstOfferFailure's text when the first
// offer is not acknowledged within the usual wait.
func waitFirstOffer(t *testing.T, store bus.Store, done <-chan error) {
	t.Helper()
	if failure := firstOfferFailure(store, done, eventuallyWait); failure != "" {
		t.Fatal(failure)
	}
}

func withoutStatePath(store bus.Store, err error) string {
	if err == nil {
		return "none"
	}
	return strings.ReplaceAll(err.Error(), store.Dir(), "<state>")
}

func TestFirstOfferFailureSaysWhyTheHostDidNotStart(t *testing.T) {
	t.Run("host ended", func(t *testing.T) {
		store, done := launchDisableHost(t, "bad-hello", true)
		// Wait until Run has ended, then hand its result on, so the check does
		// not depend on how fast the host fails.
		ended := make(chan error, 1)
		select {
		case err := <-done:
			ended <- err
		case <-time.After(eventuallyWait):
			t.Fatal("the host with a bad hello did not end")
		}
		text := firstOfferFailure(store, ended, 100*time.Millisecond)
		if !strings.Contains(text, "the host ended before its first offer was acknowledged") || !strings.Contains(text, "plugin hello failed") {
			t.Fatalf("an early host exit was not named: %q", text)
		}
		if strings.Contains(text, store.Dir()) {
			t.Fatal("the text names the state path")
		}
	})
	t.Run("host still runs", func(t *testing.T) {
		// An enabled plugin whose host never started: no result on done and no
		// checkpoint, the same view as a host that does not get to its first offer.
		store, _ := hostStore(t)
		text := firstOfferFailure(store, make(chan error), 100*time.Millisecond)
		for _, want := range []string{"the host still runs", "last status error: none", "gap pending: true", `last gap: "enabled"`} {
			if !strings.Contains(text, want) {
				t.Fatalf("a missing first ack was not described (%s): %q", want, text)
			}
		}
	})
	t.Run("state path is not named", func(t *testing.T) {
		store, _ := hostStore(t)
		got := withoutStatePath(store, fmt.Errorf("open %s/plugins/x: no such file", store.Dir()))
		if strings.Contains(got, store.Dir()) || !strings.Contains(got, "<state>/plugins/x") {
			t.Fatalf("the state path was kept: %q", got)
		}
	})
	t.Run("first offer acknowledged", func(t *testing.T) {
		store, done := launchDisableHost(t, "exit1-on-code", true)
		if text := firstOfferFailure(store, done, eventuallyWait); text != "" {
			t.Fatal(text)
		}
	})
}
