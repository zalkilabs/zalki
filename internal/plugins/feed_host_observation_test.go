package plugins

import (
	"bytes"
	"encoding/json"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type hostFeedPosition struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence,string"`
}
type hostFeedCheckpoint struct {
	LastGap string           `json:"last_gap"`
	LastAck string           `json:"last_ack"`
	NeedCut bool             `json:"need_cut"`
	Backoff int              `json:"backoff"`
	Pending *json.RawMessage `json:"pending"`
	Cursor  hostFeedPosition `json:"cursor"`
	Rows    map[string]struct {
		Class string           `json:"class"`
		Floor hostFeedPosition `json:"floor"`
	} `json:"rows"`
	States map[string]struct {
		State model.SessionState `json:"state"`
	} `json:"states"`
}

func readHostFeedCheckpoint(t *testing.T, store bus.Store, name string) (hostFeedCheckpoint, bool) {
	t.Helper()
	raw, err := statefile.ReadBytes(filepath.Join(store.Dir(), "plugins", name+".feed-state.json"), 524288)
	if os.IsNotExist(err) {
		return hostFeedCheckpoint{}, false
	}
	if err != nil {
		t.Fatal("F_HOST_BOUNDED_READONLY_CHECKPOINT", err)
	}
	var checkpoint hostFeedCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	return checkpoint, true
}
func waitHostFeedCut(t *testing.T, store bus.Store, name, reason string) {
	t.Helper()
	eventually(t, func() bool {
		c, ok := readHostFeedCheckpoint(t, store, name)
		return ok && c.LastGap == reason && c.Pending == nil && !c.NeedCut
	})
}
func waitHostFeedFloor(t *testing.T, store bus.Store, name, id string) {
	t.Helper()
	eventually(t, func() bool {
		c, ok := readHostFeedCheckpoint(t, store, name)
		row, known := c.Rows[id]
		return ok && known && row.Class == "active" && model.IsCanonicalUUID(row.Floor.Epoch) && row.Floor.Sequence > 0 && c.Pending == nil && !c.NeedCut
	})
}
func nativeHostEvent(t *testing.T, store bus.Store) (model.Event, hostFeedPosition) {
	t.Helper()
	var event model.Event
	var head bus.EventWindow
	err := bus.WithJournalRead(store, func(view bus.JournalView) error {
		var err error
		head, err = view.EventWindow()
		if err != nil {
			return err
		}
		event, err = view.EventAt(head.Epoch, head.Last)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return event, hostFeedPosition{head.Epoch, head.Last}
}
func awaitSeenHostEventAck(t *testing.T, store bus.Store, id string, position hostFeedPosition) {
	t.Helper()
	eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(hostData(store), ".seen-"+id))
		c, ok := readHostFeedCheckpoint(t, store, "fake-notifier")
		return err == nil && ok && c.LastAck == id && c.Cursor == position && c.Pending == nil
	})
}
func assertHostCutAt(t *testing.T, store bus.Store, expected hostFeedPosition) {
	t.Helper()
	eventually(t, func() bool {
		c, ok := readHostFeedCheckpoint(t, store, "fake-notifier")
		return ok && c.LastGap == "launch" && c.Pending == nil && !c.NeedCut && c.Cursor == expected
	})
}

func readExactHostGap(t *testing.T, path, id, reason string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	var gap frame
	if err != nil || json.Unmarshal(raw, &gap) != nil || gap.ID != id || gap.Gap != reason || gap.Event != nil {
		t.Fatal("F_HOST_PENDING_GAP_ID_REASON_EXACT", err)
	}
	return raw
}
func assertExactHostGapReplay(t *testing.T, path, id, reason string, want []byte) {
	t.Helper()
	if !bytes.Equal(readExactHostGap(t, path, id, reason), want) {
		t.Fatal("F_HOST_PENDING_GAP_IMMUTABLE_REPLAY_BYTES")
	}
}
func assertHostPostCutFloor(t *testing.T, store bus.Store, expected hostFeedPosition) {
	t.Helper()
	c, ok := readHostFeedCheckpoint(t, store, "fake-notifier")
	if !ok || c.Cursor != expected || c.Pending != nil || c.LastGap != "launch" {
		t.Fatal("F_HOST_POST_LAUNCH_FIRST_INTERVAL_PRIVATE_FLOOR")
	}
}

func hostConditionBeforeDeadline(condition func() bool) bool {
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
func assertHostLaunchAfterPending(t *testing.T, store bus.Store, prior, session string, cut hostFeedPosition) {
	t.Helper()
	if !hostConditionBeforeDeadline(func() bool { return laterGapSeen(hostData(store), prior, "launch") }) {
		t.Fatal("F_HOST_RESUMED_ACK_QUEUES_EXACT_LAUNCH_GAP")
	}
	assertHostCutAt(t, store, cut)
	if err := store.Update(session, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	_, postCut := nativeHostEvent(t, store)
	waitHostFeedFloor(t, store, "fake-notifier", session)
	assertHostPostCutFloor(t, store, postCut)
}
