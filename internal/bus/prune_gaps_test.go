package bus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

// These tests close gaps found in pruneRun.sweepSessions (prune.go) and
// Store.reconcileApprovalHolds (approval.go). They pin today's behavior; odd
// results are characterized, not endorsed.
//
// A prune run reports session decisions in this order (report.Sessions):
//  1. Session files that cannot be used (unsupported schema, wrong name, ...).
//  2. sweepSessions, for each session in ID order:
//     a retired session is deleted if it is older than the retention window
//     (listed in DeletedSessions, not in Sessions); otherwise, if it still
//     holds a listener lease, "lease_released" is reported and, unless this is
//     a dry run, the lease is cleared and UpdatedAt is set to now; otherwise
//     it is left alone. A live session is retired when it was selected
//     ("explicit_cleanup") or its owner exited ("owner_exited"); the reason is
//     reported before the record is saved.
//  3. reconcileApprovalHolds:
//     a. unreadable message files ("message_unreadable"), in file order;
//     b. each session in ID order. A retired (or about to be retired) session
//     with queued approvals is "approval_settled", with only a stale count
//     "approval_recounted"; a live session whose count differs from its queued
//     messages is "approval_recounted". The report entry is added before the
//     dry-run check, and a dry run reports the same entries as an applied run;
//     c. queued approvals of targets that have no session record, sorted by ID
//     ("approval_settled").
// Then orphan listener locks, flood records and watches are handled.

const (
	pruneRetention = 7 * 24 * time.Hour
	retiredAgo     = 3 * 24 * time.Hour
)

func staleListener() *model.ListenerLease {
	return &model.ListenerLease{ID: newID(), Generation: newID(), Endpoint: model.Endpoint{Path: "/fixture/not-a-real-socket"}}
}

func ageOf(t *testing.T, stamp string) time.Duration {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return time.Since(at)
}

// retiredWithLease registers a cursor session and rewrites it as a retired
// record, updated the given time ago, that still holds a listener lease (an
// older build can leave one behind).
func retiredWithLease(t *testing.T, store Store, ago time.Duration) Session {
	t.Helper()
	v, _ := register(t, store, "cursor")
	v.Retired, v.State, v.RetiredReason, v.Listener = true, model.StateRetired, "session_end", staleListener()
	v.UpdatedAt = time.Now().Add(-ago).UTC().Format(time.RFC3339Nano)
	if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
		t.Fatal(err)
	}
	return v
}

func lockFile(t *testing.T, store Store, v Session) string {
	t.Helper()
	path := store.listenerLockPath(v.Kind, v.ID)
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertSessionBytes checks that prune did not rewrite the given sessions.
func assertSessionBytes(t *testing.T, store Store, want map[string][]byte) {
	t.Helper()
	for id, content := range want {
		if string(sessionBytes(t, store, id)) != string(content) {
			t.Fatalf("prune rewrote %s, which has nothing to change", id)
		}
	}
}

// assertLeaseReleased checks a session after its lease was released: still
// retired for the same reason, no lease, UpdatedAt set to now, lock file kept.
func assertLeaseReleased(t *testing.T, store Store, id, lock string) {
	t.Helper()
	released, err := store.session(id)
	if err != nil {
		t.Fatal(err)
	}
	if released.Listener != nil || !released.Retired || released.State != model.StateRetired || released.RetiredReason != "session_end" {
		t.Fatalf("released session = %+v, want it still retired for the same reason, with no lease", released)
	}
	if age := ageOf(t, released.UpdatedAt); age < 0 || age > time.Minute {
		t.Fatalf("UpdatedAt is %v old; releasing a lease should set it to now", age)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("releasing a lease removed the lock file of a session that still exists", err)
	}
}

func TestPruneReleasesTheLeaseOfARetiredSessionThatIsNotOldEnough(t *testing.T) {
	store := fixtureStore(t)
	leased := retiredWithLease(t, store, retiredAgo)
	lock := lockFile(t, store, leased)
	tooOld := retiredWithLease(t, store, pruneRetention+time.Hour)
	noLease := retiredWithLease(t, store, retiredAgo)
	noLease.Listener = nil
	if err := store.locked(func() error { return store.saveSession(noLease) }); err != nil {
		t.Fatal(err)
	}
	live, _ := register(t, store, "cursor")
	before := sessionBytes(t, store, leased.ID)
	untouched := map[string][]byte{noLease.ID: sessionBytes(t, store, noLease.ID), live.ID: sessionBytes(t, store, live.ID)}

	preview, err := store.Prune(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []PruneItem{{ID: leased.ID, Reason: "lease_released"}}
	if !slices.Equal(preview.Sessions, want) {
		t.Fatalf("dry-run reported %v, want %v", preview.Sessions, want)
	}
	if got := preview.DeletedSessions; len(got) != 1 || got[0] != (PruneItem{ID: tooOld.ID, Reason: "retention"}) {
		t.Fatalf("dry-run deleted list = %v, want only the session older than the window", got)
	}
	assertSessionBytes(t, store, map[string][]byte{leased.ID: before})
	if _, err := os.Stat(mustSessionPath(t, store, tooOld.ID)); err != nil {
		t.Fatal("a dry run removed the old session", err)
	}

	report, err := store.Prune(false, nil)
	if err != nil || !slices.Equal(report.Sessions, want) || len(report.DeletedSessions) != 1 {
		t.Fatalf("applied prune = %+v (%v), want the dry-run decisions", report, err)
	}
	assertLeaseReleased(t, store, leased.ID, lock)
	assertSessionBytes(t, store, untouched)
	if _, err := os.Stat(mustSessionPath(t, store, tooOld.ID)); !os.IsNotExist(err) {
		t.Fatal("the session older than the window was kept", err)
	}

	again, err := store.Prune(false, nil)
	if err != nil || len(again.Sessions) != 0 || len(again.DeletedSessions) != 0 {
		t.Fatalf("a second run found more to do: %+v %v", again, err)
	}
}

// Characterized, not endorsed: releasing a lease sets UpdatedAt to now, and
// UpdatedAt is also the clock for retention. A session within the current
// retention window starts its window again; expired sessions are deleted first.
func TestPruneLeaseReleaseRestartsTheRetentionWindow(t *testing.T) {
	store := fixtureStore(t)
	leased := retiredWithLease(t, store, retiredAgo)
	lock := lockFile(t, store, leased)
	if _, err := store.Prune(false, nil); err != nil {
		t.Fatal(err)
	}
	// Three days old at the start: a two-day window would have removed it.
	report, err := store.PruneRetired(false, nil, 2*24*time.Hour)
	if err != nil || len(report.DeletedSessions) != 0 {
		t.Fatalf("a released session was deleted straight away: %+v %v", report, err)
	}
	// Age the released record explicitly, avoiding a scheduler-dependent sleep.
	aged, err := store.session(leased.ID)
	if err != nil {
		t.Fatal(err)
	}
	aged.UpdatedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if err := store.locked(func() error { return store.saveSession(aged) }); err != nil {
		t.Fatal(err)
	}
	report, err = store.PruneRetired(false, nil, time.Minute)
	if err != nil || len(report.DeletedSessions) != 1 || report.DeletedSessions[0] != (PruneItem{ID: leased.ID, Reason: "retention"}) {
		t.Fatalf("an aged session was not deleted: %+v %v", report, err)
	}
	if want := []string{filepath.Base(lock)}; !slices.Equal(report.DeletedLocks, want) {
		t.Fatalf("deleted locks = %v, want %v", report.DeletedLocks, want)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("the lock file of a deleted session remained", err)
	}
}

// queuedFor stores a message that waits for the target's permission prompt.
func queuedFor(t *testing.T, store Store, sender Session, target string) Message {
	t.Helper()
	msg := Message{
		Schema: recordSchema, ID: newID(), From: sender.ID, To: target, Body: "waits", CreatedAt: model.Timestamp(),
		Status: model.StatusApprovalBlocked, Held: true, Receipt: &Receipt{At: model.Timestamp(), Status: model.StatusApprovalBlocked, Error: model.ApprovalQueuedNote},
	}
	if current, err := store.session(target); err == nil {
		msg.TargetGeneration = current.Generation
	}
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func fixedSession(t *testing.T, store Store, last string) Session {
	t.Helper()
	v, _, err := store.Register("cursor", "00000000-0000-4000-8000-0000000000"+last, "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func withHeld(t *testing.T, store Store, v Session, held int) Session {
	t.Helper()
	v.ApprovalHeld = held
	if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPruneDryRunOnAMissingApprovalTargetMatchesTheAppliedRun(t *testing.T) {
	store := fixtureStore(t)
	sender, _ := register(t, store, "codex")
	// Two targets without a session record; their queues are settled in ID order.
	second, first := "cursor:00000000-0000-4000-8000-0000000000f2", "cursor:00000000-0000-4000-8000-0000000000f1"
	queued := []Message{queuedFor(t, store, sender, second), queuedFor(t, store, sender, first)}
	want := []PruneItem{{ID: first, Reason: "approval_settled"}, {ID: second, Reason: "approval_settled"}}

	preview, err := store.Prune(true, nil)
	if err != nil || !slices.Equal(preview.Sessions, want) {
		t.Fatalf("dry-run reported %v (%v), want %v", preview.Sessions, err, want)
	}
	for _, msg := range queued {
		if current := journalMessage(t, store, msg.ID); !current.Held || current.Status != model.StatusApprovalBlocked {
			t.Fatalf("a dry run settled %s: %+v", msg.ID, current)
		}
	}
	report, err := store.Prune(false, nil)
	if err != nil || !slices.Equal(report.Sessions, want) {
		t.Fatalf("applied run reported %v (%v), want the dry-run list %v", report.Sessions, err, want)
	}
	for _, msg := range queued {
		current := journalMessage(t, store, msg.ID)
		if current.Held || current.Status != model.StatusNotSent || current.Receipt == nil || current.Receipt.Error != "destination has retired" {
			t.Fatalf("queue of a missing target after prune = %+v", current)
		}
	}
	if again, err := store.Prune(false, nil); err != nil || len(again.Sessions) != 0 {
		t.Fatalf("a second run found more to do: %v %v", again.Sessions, err)
	}
}

func journalMessage(t *testing.T, store Store, id string) Message {
	t.Helper()
	msg, err := store.message(id)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// unreadableMessage writes malformed JSON so record decoding fails on every OS
// and user identity, including root. Permission-error coverage lives separately.
func unreadableMessage(t *testing.T, store Store) string {
	t.Helper()
	id := newID()
	path := filepath.Join(store.Dir, "messages", id+".json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	return id
}

// assertHoldCounts checks the approval count of each session. Sessions must
// not be retired before the applied run (retirement is not checked after it).
func assertHoldCounts(t *testing.T, store Store, want map[string]int, applied bool) {
	t.Helper()
	for id, held := range want {
		current, err := store.session(id)
		if err != nil || current.ApprovalHeld != held || (!applied && current.Retired) {
			t.Fatalf("session %s holds %d, retired=%v (%v); want %d (applied run: %v)", id, current.ApprovalHeld, current.Retired, err, held, applied)
		}
	}
}

func TestPruneReportsApprovalDecisionsInOrderAndDryRunMatchesApply(t *testing.T) {
	store := fixtureStore(t)
	sender, _ := register(t, store, "codex")
	stale := withHeld(t, store, fixedSession(t, store, "01"), 2)    // count 2, nothing queued
	short := withHeld(t, store, fixedSession(t, store, "02"), 0)    // count 0, one queued
	retiring := withHeld(t, store, fixedSession(t, store, "03"), 1) // selected for cleanup
	steady := withHeld(t, store, fixedSession(t, store, "04"), 1)   // count matches
	queuedFor(t, store, sender, short.ID)
	retiringMsg := queuedFor(t, store, sender, retiring.ID)
	steadyMsg := queuedFor(t, store, sender, steady.ID)
	missingB, missingA := "cursor:00000000-0000-4000-8000-0000000000f2", "cursor:00000000-0000-4000-8000-0000000000f1"
	queuedFor(t, store, sender, missingB)
	queuedFor(t, store, sender, missingA)
	unreadable := unreadableMessage(t, store)
	issue := "cursor:00000000-0000-4000-8000-0000000000e0"
	if err := os.WriteFile(mustSessionPath(t, store, issue), []byte(`{"schema":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	want := []PruneItem{
		{ID: issue, Reason: "session_unsupported_schema"}, // 1. unusable session files
		{ID: retiring.ID, Reason: "explicit_cleanup"},     // 2. sweep
		{ID: unreadable, Reason: "message_unreadable"},    // 3a. unreadable messages
		{ID: stale.ID, Reason: "approval_recounted"},      // 3b. sessions in ID order: count too high
		{ID: short.ID, Reason: "approval_recounted"},      //     count too low
		{ID: retiring.ID, Reason: "approval_settled"},     //     retiring session with a queue
		{ID: missingA, Reason: "approval_settled"},        // 3c. targets without a session, sorted
		{ID: missingB, Reason: "approval_settled"},
	}
	before := map[string]int{stale.ID: 2, short.ID: 0, retiring.ID: 1, steady.ID: 1}

	preview, err := store.Prune(true, []string{retiring.ID})
	if err != nil || !slices.Equal(preview.Sessions, want) {
		t.Fatalf("dry-run reported\n%v\n(%v), want\n%v", preview.Sessions, err, want)
	}
	assertHoldCounts(t, store, before, false)
	if !journalMessage(t, store, retiringMsg.ID).Held {
		t.Fatal("a dry run settled the queue of a session that would be retired")
	}

	report, err := store.Prune(false, []string{retiring.ID})
	if err != nil || !slices.Equal(report.Sessions, want) {
		t.Fatalf("applied run reported\n%v\n(%v), want the dry-run list\n%v", report.Sessions, err, want)
	}
	assertHoldCounts(t, store, map[string]int{stale.ID: 0, short.ID: 1, retiring.ID: 0, steady.ID: 1}, true)
	if current := journalMessage(t, store, retiringMsg.ID); current.Held || current.Status != model.StatusNotSent {
		t.Fatalf("the queue of the retired session = %+v", current)
	}
	if current := journalMessage(t, store, steadyMsg.ID); !current.Held || current.Status != model.StatusApprovalBlocked {
		t.Fatalf("a matching live queue was touched: %+v", current)
	}
}

// exitedOwner records an owned child process, then kills and reaps it. The
// deadline also bounds cleanup if a fixture operation fails before the kill.
func exitedOwner(t *testing.T, store Store, sessions ...Session) {
	t.Helper()
	selfProcess(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sleep", "60")
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	p, err := procwatch.Read(cmd.Process.Pid)
	if err != nil || p.Start == "" {
		t.Fatalf("cannot identify fixture owner: %+v %v", p, err)
	}
	ref := &model.ProcessRef{PID: p.PID, Start: p.Start, Role: "client"}
	for _, v := range sessions {
		v.Owner = ref
		if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("owner fixture exceeded its deadline: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	if processLiveness(ref) != model.LivenessExited {
		t.Fatal("the owner is still alive")
	}
}

// When selection and owner exit both apply, explicit cleanup wins. Keep an
// unselected session with the same exited owner as a positive liveness control.
func TestPruneReportsExplicitCleanupBeforeOwnerExited(t *testing.T) {
	store := fixtureStore(t)
	selected, _ := register(t, store, "cursor")
	unselected, _ := register(t, store, "cursor")
	exitedOwner(t, store, selected, unselected)
	before := map[string][]byte{
		selected.ID:   sessionBytes(t, store, selected.ID),
		unselected.ID: sessionBytes(t, store, unselected.ID),
	}
	want := []PruneItem{{ID: selected.ID, Reason: "explicit_cleanup"}, {ID: unselected.ID, Reason: "owner_exited"}}
	slices.SortFunc(want, func(a, b PruneItem) int { return strings.Compare(a.ID, b.ID) })
	for _, dry := range []bool{true, false} {
		report, err := store.Prune(dry, []string{selected.ID})
		if err != nil || !slices.Equal(report.Sessions, want) {
			t.Fatalf("dry=%v: reported %v (%v), want %v", dry, report.Sessions, err, want)
		}
		for _, item := range want {
			current, err := store.session(item.ID)
			if err != nil || current.Retired != !dry {
				t.Fatalf("dry=%v: %s retired=%v (%v)", dry, item.ID, current.Retired, err)
			}
			if !dry && (current.State != model.StateRetired || current.RetiredReason != item.Reason) {
				t.Fatalf("retirement of %s: state=%q reason=%q, want retired for %q", item.ID, current.State, current.RetiredReason, item.Reason)
			}
		}
		if dry {
			assertSessionBytes(t, store, before)
		}
	}
}
