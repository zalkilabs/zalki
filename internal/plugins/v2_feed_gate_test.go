package plugins

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zalkilabs/zalki/internal/model"
	"os"
	"testing"
	"time"
)

type v2PeerInput struct {
	raw []byte
	err error
}

func newV2PeerInput() <-chan v2PeerInput {
	input := make(chan v2PeerInput, 1)
	go func() {
		defer close(input)
		reader := bufio.NewReaderSize(os.Stdin, maxFrame+1)
		for {
			raw, err := reader.ReadBytes('\n')
			input <- v2PeerInput{raw, err}
			if err != nil {
				return
			}
		}
	}()
	return input
}
func (c v2TestPeer) helloAndGateOps(result string) error {
	if raw, err := c.next("hello"); err != nil || !bytes.Contains(raw, []byte(`"zalki-plugin/1"`)) || !bytes.Contains(raw, []byte(`"reply"`)) {
		return errors.New("missing host hello with reply")
	}
	if err := v2TestWrite(frame{Hello: &hello{Wire: wireName, Requires: []string{"feed", "inbox", "reply"}, Optional: []string{"members"}}}); err != nil {
		return err
	}
	if raw, err := c.next("hello_ok"); err != nil || !bytes.Contains(raw, []byte(`"missing_optional":["members"]`)) {
		return errors.New("missing hello_ok")
	}
	if err := os.WriteFile(result+".hello-ok", []byte("yes"), 0600); err != nil {
		return err
	}
	return c.waitOnlyOpsGate(result + ".ops-ready")
}
func (c v2TestPeer) waitOnlyOpsGate(path string) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case entry, open := <-c.in:
			if !open {
				return errors.New("fixture feed closed before op gate")
			}
			if entry.err != nil {
				return entry.err
			}
			if err := c.ackGatedFrame(entry.raw); err != nil {
				return err
			}
		case <-tick.C:
		}
	}
}
func (c v2TestPeer) ackGatedFrame(raw []byte) error {
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	if _, present := f["id"]; !present {
		return errors.New("unexpected host frame before op release")
	}
	return c.ackOffer(f)
}
func (x *v2Fixture) primeV2MetadataFeed(t *testing.T, host <-chan error) {
	t.Helper()
	eventually(t, func() bool { _, err := os.Stat(x.result + ".hello-ok"); return err == nil })
	waitPrimeFeed(t, x, host, "initial cut", v2FeedCutSettled)
	t.Log("F_V2_PHASE_INITIAL_GAP_ACK_PRIVATE_CUT")
	if err := x.store.Update(x.asker.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	waitPrimeFeed(t, x, host, "busy floor", func(c hostFeedCheckpoint, found bool) bool { return v2FeedFloorSettled(c, found, x.asker.ID) })
	t.Log("F_V2_PHASE_ACTUAL_BUSY_PRIVATE_FLOOR")
	if err := x.store.Update(x.asker.ID, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
	waitPrimeFeed(t, x, host, "ready event", func(c hostFeedCheckpoint, found bool) bool { return v2FeedReadySettled(c, found, x.asker.ID) })
	t.Log("F_V2_PHASE_READY_EVENT_ACK_BEFORE_OPS")
	if err := os.WriteFile(x.result+".ops-ready", []byte("yes"), 0600); err != nil {
		t.Fatal(err)
	}
}

// The three priming steps wait for these checkpoint conditions.
func v2FeedCutSettled(c hostFeedCheckpoint, found bool) bool {
	return found && c.LastGap == "enabled" && c.Pending == nil && !c.NeedCut
}

func v2FeedFloorSettled(c hostFeedCheckpoint, found bool, id string) bool {
	row, known := c.Rows[id]
	return found && known && row.Class == "active" && model.IsCanonicalUUID(row.Floor.Epoch) && row.Floor.Sequence > 0 && c.Pending == nil && !c.NeedCut
}

func v2FeedReadySettled(c hostFeedCheckpoint, found bool, id string) bool {
	seen, known := c.States[id]
	return found && known && seen.State == model.StateReady && c.Pending == nil && !c.NeedCut
}

// waitPrimeFeed polls the host feed checkpoint until ok holds. A timeout names
// the step, the last checkpoint seen and whether the host had already
// returned, so a failure on a slow runner says what the feed was doing.
func waitPrimeFeed(t *testing.T, x *v2Fixture, host <-chan error, step string, ok func(hostFeedCheckpoint, bool) bool) {
	t.Helper()
	var last hostFeedCheckpoint
	var found bool
	if eventuallyWithin(eventuallyWait, func() bool {
		last, found = readHostFeedCheckpoint(t, x.store, "arbiter")
		return ok(last, found)
	}) {
		return
	}
	t.Fatalf("v2 feed step %q did not settle: %s; %s", step, describeHostFeedCheckpoint(last, found, x.asker.ID), v2HostReturned(host))
}

func describeHostFeedCheckpoint(c hostFeedCheckpoint, found bool, id string) string {
	pending := "none"
	if c.Pending != nil {
		var p struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(*c.Pending, &p)
		pending = p.Kind
	}
	state := "unknown"
	if seen, known := c.States[id]; known {
		state = string(seen.State)
	}
	row := "none"
	if r, known := c.Rows[id]; known {
		row = r.Class
	}
	return fmt.Sprintf("last checkpoint: found=%v last_gap=%q backoff=%d need_cut=%v pending=%s cursor=%d state=%s row=%s",
		found, c.LastGap, c.Backoff, c.NeedCut, pending, c.Cursor.Sequence, state, row)
}

func v2HostReturned(host <-chan error) string {
	select {
	case err := <-host:
		return fmt.Sprintf("the host had already returned: %v", err)
	default:
		return "the host was still running"
	}
}
