package pluginfeed

import (
	"bytes"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestStreamPlainFinalHandoffAndAckRecheckPermission(t *testing.T) {
	for _, mode := range []string{"handoff", "ack"} {
		t.Run(mode, func(t *testing.T) { checkPlainFinalPermission(t, mode) })
	}
}

func checkPlainFinalPermission(t *testing.T, mode string) {
	t.Helper()
	feed, launch, from, key, offer := streamPendingMessageFixture(t)
	if mode == "ack" {
		streamAttemptAll(t, feed, launch, offer)
	}
	before := readStreamCheckpoint(t, feed).Cursor
	owner := readCommitFile(t, feed.pluginPath())
	if err := bus.SetSessionPlugin(feed.store, from.ID, key, launch.Name, true); err != nil {
		t.Fatal(err)
	}
	if mode == "handoff" {
		denied, err := feed.NextPluginFeed(t.Context(), launch, offer.ID, 1)
		assertPlainFinalHandoffDenied(t, denied, offer.ID, err)
	} else {
		pending, err := feed.AckPluginFeed(launch, offer.ID)
		if err != nil || pending == "" || pending == offer.ID {
			t.Fatal("F_PLAIN_FINAL_ACK_RECHECKS_CURRENT_PERMISSION", pending, err)
		}
	}
	state := readStreamCheckpoint(t, feed)
	if state.Pending == nil || state.Pending.Kind != "gap" || state.LastGap != "policy" || state.Cursor != before || len(state.States) != 0 || len(state.Seen) != 0 {
		t.Fatal("F_PLAIN_POLICY_RESET_DROPS_ACK_EVIDENCE")
	}
	if !bytes.Equal(owner, readCommitFile(t, feed.pluginPath())) {
		t.Fatal("F_PLAIN_FINAL_CHECK_NEVER_READMITS_OR_REBINDS")
	}
}

func assertPlainFinalHandoffDenied(t *testing.T, denied PluginDelivery, old string, err error) {
	t.Helper()
	if err != nil || denied.ID == old || denied.ID == "" || denied.Gap != "policy" || denied.Event != nil {
		t.Fatal("F_PLAIN_FINAL_HANDOFF_RECHECKS_CURRENT_PERMISSION", err)
	}
	for _, raw := range denied.Frames {
		if len(raw) != 0 {
			t.Fatal("F_PLAIN_FINAL_HANDOFF_RECHECKS_CURRENT_PERMISSION")
		}
	}
}
