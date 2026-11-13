package pluginfeed

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// The feed shows a message to a plugin only through its feed sessions. The
// other party is reduced to its kind, so no frame names a session or plugin
// outside the feed. reply_to is always kept: a reply's parties are its
// parent's parties reversed (bus checkReplyParties), so the parent is eligible
// for the same feed set.
func TestFeedViewMessageMatrix(t *testing.T) {
	a, c := "codex:00000000-0000-4000-8000-00000000000a", "claude:00000000-0000-4000-8000-00000000000c"
	b, d := "codex:00000000-0000-4000-8000-00000000000b", "cursor:00000000-0000-4000-8000-00000000000d"
	x := "plugin:00000000-0000-4000-8000-0000000000ee"
	feed := []string{a, c}
	outside := []string{b, d, x, strings.TrimPrefix(b, "codex:"), strings.TrimPrefix(d, "cursor:"), strings.TrimPrefix(x, "plugin:")}
	for _, tc := range []feedViewCase{
		{"A to C", a, c, true, a, c, true},
		{"A asks B", a, b, true, a, "codex:", true},
		{"B replies to A", b, a, true, "codex:", a, true},
		{"B to D", b, d, false, "", "", false},
		{"A to plugin", a, x, true, a, "plugin:", true},
		{"B to plugin", b, x, false, "", "", false},
		{"A to A", a, a, true, a, a, true},
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
	got, ok := feedView(listMembers(feed), true, event)
	if ok != tc.offered {
		t.Fatalf("offered = %v, want %v", ok, tc.offered)
	}
	if *event.Message != stored {
		t.Fatal("feedView changed the stored event")
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

// Session events are offered only for member sessions, unchanged.
func TestFeedViewSessionEvents(t *testing.T) {
	a, b := "codex:00000000-0000-4000-8000-00000000000a", "codex:00000000-0000-4000-8000-00000000000b"
	event := model.Event{Type: model.EventSessionStateChanged, Session: model.EventSession{ID: a, Cause: model.CauseStop}}
	if got, ok := feedView(listMembers([]string{a}), true, event); !ok || got.Session != event.Session {
		t.Fatalf("feed session event = %+v, %v", got, ok)
	}
	if _, ok := feedView(listMembers([]string{b}), true, event); ok {
		t.Fatal("offered a session event outside the feed")
	}
}
