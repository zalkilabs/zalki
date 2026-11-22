package plugins

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// ackScript answers each ack with the next result (the last one repeats) and
// counts the calls. The loop test calls it from another goroutine.
type ackScript struct {
	mu      sync.Mutex
	results []error
	calls   int
}

func (a *ackScript) feed(*pluginfeed.PluginFeedStatus, string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if len(a.results) == 0 {
		return "", nil
	}
	err := a.results[0]
	if len(a.results) > 1 {
		a.results = a.results[1:]
	}
	return "", err
}

func (a *ackScript) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// v2AckHost builds a host with a scripted feed ack and a pipe for its frames.
func v2AckHost(t *testing.T, script *ackScript) (*v2Host, *bufio.Reader) {
	t.Helper()
	x := v2HostFixture(t)
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rd.Close(); wr.Close() })
	h := &v2Host{ctx: t.Context(), feed: x.feed, binding: &x.status, child: &child{input: wr}, enabled: []string{"feed"},
		acks: make(chan string, 1), ackFeed: script.feed}
	return h, bufio.NewReader(rd)
}

func TestV2HostHoldsAnAckThatRanOutOfWorkAndRetriesOnTheNextTick(t *testing.T) {
	script := &ackScript{results: []error{pluginfeed.ErrFeedWork, pluginfeed.ErrFeedWork, nil}}
	h, _ := v2AckHost(t, script)
	h.offer, h.offerAt = &pluginfeed.PluginDelivery{ID: "offer-1"}, time.Now().Add(-time.Hour)
	if err := h.ack("offer-1"); err != nil || h.heldAck != "offer-1" || !h.ackHeld.Load() {
		t.Fatal("an ack that ran out of work was not held", err)
	}
	if h.queue(&v2Command{Ack: "offer-2"}) {
		t.Fatal("a second ack was accepted while one is held")
	}
	if err := h.tick(); err != nil || script.count() != 2 || h.heldAck != "offer-1" || h.offer == nil {
		t.Fatal("the held ack was not retried on the tick, or the offer timed out while it waited", err, script.count())
	}
	if err := h.tick(); err != nil || script.count() != 3 || h.heldAck != "" || h.ackHeld.Load() || h.offer != nil {
		t.Fatal("the held ack was not used on the retry", err, script.count())
	}
	if !h.queue(&v2Command{Ack: "offer-2"}) {
		t.Fatal("the next ack was refused after the held one was used")
	}
}

func TestV2HostOtherAckErrorsStayTerminal(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{"unavailable", model.NewError(model.CodeUnavailable, "feed down"), "unavailable"},
		{"conflict", model.NewError(model.CodeConflict, "not handed off"), "protocol_error"},
		{"plain error", errors.New("disk"), "unavailable"},
	} {
		h, frames := v2AckHost(t, &ackScript{results: []error{test.err}})
		if err := h.ack("offer-1"); err == nil || h.heldAck != "" || h.ackHeld.Load() {
			t.Fatalf("%s: the ack was held or the host kept running: %v", test.name, err)
		}
		line, err := frames.ReadString('\n')
		if err != nil || line != `{"code":"`+test.code+`"}`+"\n" {
			t.Fatalf("%s: terminal frame %q, %v", test.name, line, err)
		}
	}
}

func TestV2HostStopsWhenAHeldAckFailsForAnotherReason(t *testing.T) {
	script := &ackScript{results: []error{pluginfeed.ErrFeedWork, model.NewError(model.CodeConflict, "stale")}}
	h, frames := v2AckHost(t, script)
	if err := h.ack("offer-1"); err != nil || h.heldAck == "" {
		t.Fatal("the ack was not held", err)
	}
	if err := h.tick(); err == nil || h.heldAck != "" || h.ackHeld.Load() {
		t.Fatal("a held ack that became stale did not stop the host", err)
	}
	if line, err := frames.ReadString('\n'); err != nil || line != `{"code":"protocol_error"}`+"\n" {
		t.Fatalf("terminal frame %q, %v", line, err)
	}
}

func TestV2HostStopsWhenAHeldAckWasRevoked(t *testing.T) {
	script := &ackScript{results: []error{pluginfeed.ErrFeedWork, model.NewError(model.CodeConflict, "not handed off")}}
	h, frames := v2AckHost(t, script)
	if err := h.ack("offer-1"); err != nil || h.heldAck == "" {
		t.Fatal("the ack was not held", err)
	}
	if err := h.feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := h.tick(); !errors.Is(err, errDisabled) || !h.requested {
		t.Fatal("a revoked plugin with a held ack was not stopped as a requested stop", err)
	}
	if line, err := frames.ReadString('\n'); err != nil || line != `{"code":"revoked"}`+"\n" {
		t.Fatalf("terminal frame %q, %v", line, err)
	}
}

func TestV2HostStopsWhenTheAckStaysOutOfWorkTooLong(t *testing.T) {
	script := &ackScript{results: []error{pluginfeed.ErrFeedWork}}
	h, frames := v2AckHost(t, script)
	if err := h.ack("offer-1"); err != nil {
		t.Fatal(err)
	}
	h.heldSince = time.Now().Add(-v2AckRetryWindow - time.Second)
	if err := h.tick(); err == nil {
		t.Fatal("the host kept an ack longer than the retry window")
	}
	if line, err := frames.ReadString('\n'); err != nil || line != `{"code":"unavailable"}`+"\n" {
		t.Fatalf("terminal frame %q, %v", line, err)
	}
}

func TestV2HostLoopStaysResponsiveWhileAnAckIsHeld(t *testing.T) {
	script := &ackScript{results: []error{pluginfeed.ErrFeedWork}}
	h, _ := v2AckHost(t, script)
	ctx, cancel := context.WithCancel(t.Context())
	h.ctx = ctx
	h.ops = make(chan *v2Command, 1)
	h.failed = make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- h.loop(io.Discard, "ready") }()
	h.acks <- "offer-1"
	deadline := time.Now().Add(5 * time.Second)
	for script.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if script.count() < 3 {
		t.Fatal("the loop did not retry the held ack on its ticks", script.count())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("the loop returned the wrong error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a held ack kept the loop from stopping on cancel")
	}
}
