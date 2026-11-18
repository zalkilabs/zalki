package pluginfeed

import (
	"bytes"
	"testing"
)

func TestStreamInboxReadConsumeAndSendDoNotCreditFeedParent(t *testing.T) {
	x := newReplyFixture(t)
	launch := streamTestLaunch(t, x.f)
	streamReady(t, x.f, launch)
	primeStreamSession(t, x.f.store, x.f, launch, x.from.ID)
	before := checkpointImage(t, x.f)
	raw, err := x.f.ReadPluginInboxMessage(x.message.ID)
	if err != nil || !bytes.Contains(raw, []byte(x.message.Body)) {
		t.Fatal("operation positive did not read actual inbox body", err)
	}
	reply, err := x.reply("feed-proof-one", x.message.ID, x.from.ID)
	if err != nil || reply.ReplyTo != x.message.ID {
		t.Fatal("operation positive did not send actual reply", err)
	}
	if code := x.f.ConsumePluginInbox(&x.status, x.message.ID); code != "ok" {
		t.Fatal("operation positive did not consume inbox", code)
	}
	if !bytes.Equal(before, checkpointImage(t, x.f)) || len(readStreamCheckpoint(t, x.f).Seen) != 0 {
		t.Fatal("F_OPERATION_RESULTS_NEVER_CREDIT_FEED_ACK_PROOF")
	}
	offer, frame := streamTake(t, x.f, launch)
	assertUnseenFeedReply(t, frame, reply.ID, "plugin:"+launch.ID, x.from.ID)

	// Preparation and even a preflighted handoff remain distinct from ACK.
	if len(readStreamCheckpoint(t, x.f).Seen) != 0 {
		t.Fatal("F_ONLY_OFFERED_MESSAGE_IS_NOT_FEED_ACK_EVIDENCE")
	}
	if _, err := x.reply("feed-proof-two", x.message.ID, x.from.ID); err != nil {
		t.Fatal(err)
	}
	if len(readStreamCheckpoint(t, x.f).Seen) != 0 {
		t.Fatal("F_SEND_RESULT_IS_NOT_FEED_ACK_EVIDENCE")
	}
	acknowledgeStream(t, x.f, launch, offer.ID)
	seen := readStreamCheckpoint(t, x.f).Seen
	if len(seen) != 1 || seen[0].ID != reply.ID {
		t.Fatal("F_FEED_ACK_ALONE_CREDITS_EXACT_MESSAGE")
	}
}

func assertUnseenFeedReply(t *testing.T, frame streamFrame, id, from, to string) {
	t.Helper()
	if frame.Event == nil || frame.Event.Message == nil || frame.Event.Message.ID != id || frame.Event.Message.ReplyTo != "" || frame.Event.Message.From != from || frame.Event.Message.To != to {
		t.Fatal("F_INBOX_READ_SEND_UNSEEN_PARENT_OMITTED_FROM_FEED", frame)
	}
}
