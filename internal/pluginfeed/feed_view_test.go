package pluginfeed

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// The production projection shows only eligible peers and omits a parent
// link until its actual feed ACK evidence exists in this view.
func TestFeedViewMessageMatrix(t *testing.T) {
	a, c := "codex:00000000-0000-4000-8000-00000000000a", "claude:00000000-0000-4000-8000-00000000000c"
	b, d := "codex:00000000-0000-4000-8000-00000000000b", "cursor:00000000-0000-4000-8000-00000000000d"
	x := "plugin:00000000-0000-4000-8000-0000000000ee"
	feed := []string{a, c}
	outside := []string{b, d, x, strings.TrimPrefix(b, "codex:"), strings.TrimPrefix(d, "cursor:"), strings.TrimPrefix(x, "plugin:")}
	for _, tc := range []feedViewCase{
		{"A to C", a, c, true, a, c, false},
		{"A asks B", a, b, true, a, "codex:", false},
		{"B replies to A", b, a, true, "codex:", a, false},
		{"B to D", b, d, false, "", "", false},
		{"A to plugin", a, x, true, a, "plugin:", false},
		{"B to plugin", b, x, false, "", "", false},
		{"A to A", a, a, true, a, a, false},
	} {
		t.Run(tc.name, func(t *testing.T) { checkFeedViewCase(t, feed, outside, tc) })
	}
}

const feedViewParent = "00000000-0000-4000-8000-0000000000f1"

type feedViewCase struct {
	name, from, to   string
	offered          bool
	wantFrom, wantTo string
	keepsReply       bool
}

// checkFeedViewCase offers one message event and checks the decision, the
// stored event, the shown parties and reply link, and the frame text.
func checkFeedViewCase(t *testing.T, feed, outside []string, tc feedViewCase) {
	t.Helper()
	event := model.Event{ID: "00000000-0000-4000-8000-000000000001", Type: model.EventMessageStatusChanged,
		OccurredAt: "2026-11-11T00:00:00Z", TransactionID: "00000000-0000-4000-8000-000000000002",
		Message: &model.EventMessage{ID: "00000000-0000-4000-8000-000000000003", ReplyTo: feedViewParent,
			From: tc.from, To: tc.to, Status: model.StatusSubmitted}}
	stored := *event.Message
	got := event
	pending := &streamPending{}
	ok := projectStreamMessage(&pluginStreamState{}, pluginRecord{}, &got, pending, map[string]streamDependency{}, listMembers(feed))
	if ok != tc.offered {
		t.Fatalf("offered = %v, want %v", ok, tc.offered)
	}
	if *event.Message != stored {
		t.Fatal("projection changed the stored event")
	}
	if !ok {
		return
	}
	m := got.Message
	if m.From != tc.wantFrom || m.To != tc.wantTo || (m.ReplyTo == feedViewParent) != tc.keepsReply || got.ID != event.ID {
		t.Fatalf("offered = %+v", m)
	}
	assertFrameNamesNone(t, got, outside)
}

// assertFrameNamesNone fails when the encoded event contains any outside id.
func assertFrameNamesNone(t *testing.T, event model.Event, outside []string) {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range outside {
		if strings.Contains(string(raw), id) {
			t.Fatalf("frame names %s outside the feed: %s", id, raw)
		}
	}
}

// listMembers is a member function of a fixed set of sessions.
func listMembers(ids []string) func(string) bool {
	return func(id string) bool { return slices.Contains(ids, id) }
}

// Session facts require current permission and the exact generation; the
// separate evidence projection controls whether previous state is disclosed.
func TestFeedViewSessionEvents(t *testing.T) {
	generation := model.NewUUID()
	event := model.EventSession{ID: "codex:" + model.NewUUID(), Generation: generation, State: model.StateBusy, Cause: model.CauseStop}
	if !streamSessionEventEligible(event, streamGuard{Generation: generation}, true) {
		t.Fatal("current session positive")
	}
	if streamSessionEventEligible(event, streamGuard{Generation: generation}, false) || streamSessionEventEligible(event, streamGuard{Generation: model.NewUUID()}, true) {
		t.Fatal("session outside current guard")
	}
}
