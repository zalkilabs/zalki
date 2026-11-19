package pluginfeed

import (
	"bytes"
	"encoding/json"
	"github.com/zalkilabs/zalki/internal/model"
	"os"
	"testing"
)

func retirementPreimageFixture(t *testing.T) (PluginFeed, *PluginFeedStatus, PluginDelivery) {
	t.Helper()
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	if err := store.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	offer := streamInspect(t, feed, launch)
	if pending := readStreamCheckpoint(t, feed).Pending; pending == nil || pending.Retirement == nil || !validPluginSHA(pending.Retirement.BeforeDigest) {
		t.Fatal("F_RETIREMENT_REAL_PREIMAGE_FIXTURE")
	}
	return feed, launch, offer
}
func TestStreamRetirementPreimageFormatRequired(t *testing.T) {
	feed, _, _ := retirementPreimageFixture(t)
	pending := readStreamCheckpoint(t, feed).Pending
	var frame streamFrame
	if err := json.Unmarshal(pending.Frames[0], &frame); err != nil {
		t.Fatal(err)
	}
	if !validStreamRetirement(pending.Retirement, *frame.Event) {
		t.Fatal("F_RETIREMENT_PREIMAGE_FORMAT_POSITIVE")
	}
	for _, bad := range []string{"", "bad", "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"} {
		pending.Retirement.BeforeDigest = bad
		if validStreamRetirement(pending.Retirement, *frame.Event) {
			t.Fatal("F_RETIREMENT_PREIMAGE_FORMAT_REQUIRED")
		}
	}
}
func TestStreamRetirementPreimageCheckedAtHandoffAndAck(t *testing.T) {
	for _, tc := range []struct{ stage, kind string }{{"handoff", "missing"}, {"handoff", "malformed"}, {"handoff", "wrong"}, {"ack", "missing"}, {"ack", "malformed"}, {"ack", "wrong"}} {
		t.Run(tc.stage+"_"+tc.kind, func(t *testing.T) { checkRetirementPreimage(t, tc.stage, tc.kind) })
	}
}
func checkRetirementPreimage(t *testing.T, stage, kind string) {
	t.Helper()
	feed, launch, offer := retirementPreimageFixture(t)
	if stage == "ack" {
		streamAttemptAll(t, feed, launch, offer)
	}
	before := readStreamCheckpoint(t, feed)
	raw := corruptRetirementPreimage(t, feed, kind)
	if stage == "handoff" {
		current, err := feed.NextPluginFeed(t.Context(), launch, offer.ID, 1)
		if kind != "wrong" {
			assertClosedPreimageRecord(t, feed, raw, err)
			return
		}
		if err != nil || current.ID == offer.ID || current.Gap != "policy" {
			t.Fatal("F_RETIREMENT_WRONG_PREIMAGE_HANDOFF_REFUSED", err)
		}
	} else {
		pending, err := feed.AckPluginFeed(launch, offer.ID)
		if kind != "wrong" {
			assertClosedPreimageRecord(t, feed, raw, err)
			return
		}
		after := readStreamCheckpoint(t, feed)
		if err != nil || after.Pending == nil || pending != after.Pending.ID || after.LastGap != "policy" || after.LastAck != before.LastAck || after.Cursor != before.Cursor {
			t.Fatal("F_RETIREMENT_WRONG_PREIMAGE_ACK_REFUSED", err)
		}
	}
}
func assertClosedPreimageRecord(t *testing.T, feed PluginFeed, want []byte, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("F_RETIREMENT_BAD_PREIMAGE_RECORD_CLOSED")
	}
	got, readErr := os.ReadFile(feed.streamPath())
	if readErr != nil || !bytes.Equal(got, want) {
		t.Fatal("F_RETIREMENT_BAD_PREIMAGE_NO_OPTIMISTIC_REPAIR", readErr)
	}
}
func corruptRetirementPreimage(t *testing.T, feed PluginFeed, kind string) []byte {
	t.Helper()
	raw := readCommitFile(t, feed.streamPath())
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	proof := fields["pending"].(map[string]any)["retirement"].(map[string]any)
	switch kind {
	case "missing":
		delete(proof, "before_digest")
	case "malformed":
		proof["before_digest"] = "bad"
	case "wrong":
		old := proof["before_digest"].(string)
		first := "0"
		if old[0] == '0' {
			first = "1"
		}
		proof["before_digest"] = first + old[1:]
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(feed.streamPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestStreamRetirementWireKeepsBothImageDigestsPrivate(t *testing.T) {
	feed, launch, offer := retirementPreimageFixture(t)
	proof := readStreamCheckpoint(t, feed).Pending.Retirement
	parts := streamAttemptAll(t, feed, launch, offer)
	if len(parts) != 1 || len(parts[0]) == 0 {
		t.Fatal("F_RETIREMENT_DIGEST_PRIVACY_REAL_FRAME_POSITIVE")
	}
	for _, private := range []string{"before_digest", proof.BeforeDigest, proof.Digest} {
		if bytes.Contains(parts[0], []byte(private)) {
			t.Fatal("F_RETIREMENT_IMAGES_NEVER_CHILD_FRAME")
		}
	}
	var frame streamFrame
	if err := json.Unmarshal(parts[0], &frame); err != nil || frame.Event == nil || frame.Event.ID != proof.Event || !frame.Event.Session.Retired {
		t.Fatal("F_RETIREMENT_DIGEST_PRIVACY_REAL_EVENT_POSITIVE", err)
	}
}
