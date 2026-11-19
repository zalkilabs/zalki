package plugins

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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
func (x *v2Fixture) primeV2MetadataFeed(t *testing.T) {
	t.Helper()
	eventually(t, func() bool { _, err := os.Stat(x.result + ".hello-ok"); return err == nil })
	waitHostFeedCut(t, x.store, "arbiter", "enabled")
	t.Log("F_V2_PHASE_INITIAL_GAP_ACK_PRIVATE_CUT")
	if err := x.store.Update(x.asker.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	waitHostFeedFloor(t, x.store, "arbiter", x.asker.ID)
	t.Log("F_V2_PHASE_ACTUAL_BUSY_PRIVATE_FLOOR")
	if err := x.store.Update(x.asker.ID, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		c, ok := readHostFeedCheckpoint(t, x.store, "arbiter")
		seen, known := c.States[x.asker.ID]
		return ok && known && seen.State == model.StateReady && c.Pending == nil && !c.NeedCut
	})
	t.Log("F_V2_PHASE_READY_EVENT_ACK_BEFORE_OPS")
	if err := os.WriteFile(x.result+".ops-ready", []byte("yes"), 0600); err != nil {
		t.Fatal(err)
	}
}
