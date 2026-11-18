package pluginfeed

import (
	"encoding/json"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamMessageProjectionRedactsHiddenEndpointBeforePersistence(t *testing.T) {
	feed, _, _, _, _ := streamPendingMessageFixture(t)
	state := readStreamCheckpoint(t, feed)
	var frame streamFrame
	if json.Unmarshal(state.Pending.Frames[0], &frame) != nil || frame.Event == nil || frame.Event.Message == nil {
		t.Fatal("message fixture")
	}
	from, to := frame.Event.Message.From, frame.Event.Message.To
	delete(state.Rows, to)
	pending := &streamPending{ID: frame.ID, After: state.Pending.After, Guards: state.Pending.Guards}
	if !projectStreamMessage(&state, feedRecordNamed(t, feed), frame.Event, pending, pending.Guards, func(id string) bool { return id == from }) || frame.Event.Message.From != from || frame.Event.Message.To != "codex:" {
		t.Fatal("F_METADATA_ENDPOINT_KIND_ONLY_REDACTION")
	}
}

func TestStreamMessageProjectionOmitsUnseenParentBeforePersistence(t *testing.T) {
	feed, _, _, _, _ := streamPendingMessageFixture(t)
	state := readStreamCheckpoint(t, feed)
	var frame streamFrame
	if json.Unmarshal(state.Pending.Frames[0], &frame) != nil || frame.Event == nil || frame.Event.Message == nil {
		t.Fatal("message fixture")
	}
	message := frame.Event.Message
	message.ReplyTo = model.NewUUID()
	pending := &streamPending{ID: frame.ID, After: state.Pending.After, Guards: state.Pending.Guards}
	if !projectStreamMessage(&state, feedRecordNamed(t, feed), frame.Event, pending, pending.Guards, func(id string) bool { return id == message.From || id == message.To }) || frame.Event.Message.ReplyTo != "" {
		t.Fatal("F_METADATA_UNSEEN_PARENT_OMITTED_BEFORE_PERSISTENCE")
	}
}
