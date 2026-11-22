package mcpserver

import (
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func heldMessage(at string) model.Message {
	return model.Message{
		ID: fakeMessageID, To: "codex:" + fakeMessageID, Status: model.StatusApprovalBlocked, Held: true,
		Receipt: &model.Receipt{At: at, Status: model.StatusApprovalBlocked, Error: model.ApprovalQueuedNote},
	}
}

// The wait is computed from the stored hold time when the answer is built:
// local time with offset, and for a minute or more the age.
func TestWaitingSinceShowsLocalStartAndAge(t *testing.T) {
	start := time.Date(2026, 11, 20, 22, 51, 12, 500, time.UTC)
	for _, test := range []struct {
		age  time.Duration
		want string
	}{
		{20 * time.Second, "since 2026-11-21T00:51:12+02:00"},
		{59 * time.Second, "since 2026-11-21T00:51:12+02:00"},
		{60 * time.Second, "since 2026-11-21T00:51:12+02:00 (1 min)"},
		{119 * time.Second, "since 2026-11-21T00:51:12+02:00 (1 min)"},
		{120 * time.Second, "since 2026-11-21T00:51:12+02:00 (2 min)"},
		{5 * time.Minute, "since 2026-11-21T00:51:12+02:00 (5 min)"},
		{16*time.Hour + 3*time.Minute, "since 2026-11-21T00:51:12+02:00 (16 h 3 min)"},
		{40 * time.Hour, "since 2026-11-21T00:51:12+02:00 (40 h)"},
	} {
		clock = func() time.Time { return start.Add(test.age) }
		if got := waitingSince(heldMessage(start.Format(time.RFC3339Nano))); got != test.want {
			t.Errorf("age %s: %q, want %q", test.age, got, test.want)
		}
	}
	clock = time.Now
	if waitingSince(model.Message{}) != "" || waitingSince(heldMessage("not a time")) != "" {
		t.Fatal("a message without a usable hold time got a wait text")
	}
}

func TestHeldReceiptAndViewNameTheWait(t *testing.T) {
	start := time.Date(2026, 11, 20, 22, 51, 12, 0, time.UTC)
	clock = func() time.Time { return start.Add(16*time.Hour + 3*time.Minute) }
	defer func() { clock = time.Now }()
	msg := heldMessage(start.Format(time.RFC3339Nano))
	receipt, err := messageReceipt(msg, nil)
	if err != nil || receipt.Note != "Queued until the target's permission prompt is answered; waiting since 2026-11-21T00:51:12+02:00 (16 h 3 min)." {
		t.Fatalf("receipt note = %q, %v", receipt.Note, err)
	}
	view := messageViews([]model.Message{msg}, "", nil)[0]
	if view.Error != "Waiting for the target's permission prompt since 2026-11-21T00:51:12+02:00 (16 h 3 min)." || view.Status != model.StatusQueuedForApproval {
		t.Fatalf("view error = %q, status %s", view.Error, view.Status)
	}
}

func TestListAgentsApprovalSinceIsLocalTime(t *testing.T) {
	rows := localAgents([]bus.AgentInfo{{ID: "codex:x", State: model.StateApproval, UpdatedAt: "2026-11-20T22:51:12Z", ApprovalSince: "2026-11-20T22:51:12.123456789Z"}, {ID: "cursor:y"}})
	if rows[0].ApprovalSince != "2026-11-21T00:51:12+02:00" || rows[1].ApprovalSince != "" {
		t.Fatalf("approval_since = %q and %q", rows[0].ApprovalSince, rows[1].ApprovalSince)
	}
}
