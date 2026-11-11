package bus

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// markOpen writes the open marker for id, as saveMessageRecord does before it
// writes an open record.
func markOpen(t *testing.T, store Store, id string) {
	t.Helper()
	if err := markers(store).add(id, false); err != nil {
		t.Fatal(err)
	}
}

// openMarkers lists the marker names in ID order.
func openMarkers(t *testing.T, store Store) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store.Dir, openMessagesDir))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func requireMarkers(t *testing.T, store Store, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := openMarkers(t, store); !slices.Equal(got, want) {
		t.Fatalf("open markers = %v, want %v", got, want)
	}
}

// openFixture is a codex sender and a cursor (hook delivery) target whose
// keys are both kept, so tests can send, list and acknowledge.
type openFixture struct {
	emitFixture
	targetKey string
}

func newOpenFixture(t *testing.T) openFixture {
	t.Helper()
	store := fixtureStore(t)
	sender, key, target, targetKey := registerPair(t, store, "codex", "cursor")
	return openFixture{emitFixture{store: store, target: target, sender: sender, key: key}, targetKey}
}

// record builds a message to the target with the given status, not saved.
func (f openFixture) record(status model.DeliveryStatus) model.Message {
	at := model.Timestamp()
	return model.Message{
		Schema: recordSchema, ID: newID(), From: f.sender.ID, To: f.target.ID, Body: string(status),
		TargetGeneration: f.target.Generation, CreatedAt: at, Status: status,
		Receipt: &model.Receipt{At: at, Status: status},
	}
}

// closed stores a submitted message to the target, which no hook acts on. It
// writes the record directly, so the flood limit does not apply.
func (f openFixture) closed(t *testing.T) model.Message {
	t.Helper()
	msg := f.record(model.StatusSubmitted)
	if err := f.store.locked(func() error { return f.store.saveMessage(msg) }); err != nil {
		t.Fatal(err)
	}
	return msg
}

// loseMarkers removes every marker, as if the directory was lost.
func loseMarkers(t *testing.T, store Store) {
	t.Helper()
	dir := filepath.Join(store.Dir, openMessagesDir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMarkerFollowsTheMessageState(t *testing.T) {
	f := newOpenFixture(t)
	closed := f.closed(t)
	reserved, _, _, err := f.store.ReserveMessage(SendRequest{Actor: f.sender.ID, Capability: f.key, To: f.target.ID, MessageID: newID(), Body: "attempting"})
	if err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, f.store)
	emittedOnce := f.queue(t, "emitted", time.Now())
	acked := f.queue(t, "acked", time.Now())
	requireMarkers(t, f.store, emittedOnce.ID, acked.ID)
	if _, err := f.store.Ack(f.target.ID, f.targetKey, acked.ID); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, f.store, emittedOnce.ID)
	if text, err := f.store.EmitPending(f.target.ID); err != nil || !strings.Contains(text, emittedOnce.ID) {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	requireMarkers(t, f.store)
	for _, id := range []string{closed.ID, reserved.ID} {
		if _, err := os.Lstat(markers(f.store).path(id)); !os.IsNotExist(err) {
			t.Fatal("a message no hook acts on has a marker", id, err)
		}
	}
}

// An approval hold is open until the hook releases it.
func TestOpenMarkerCoversAnApprovalHold(t *testing.T) {
	service, target, held := heldFixture(t)
	requireMarkers(t, service.Store, held.ID)
	if text, err := service.Store.EmitPending(target.ID); err != nil || !strings.Contains(text, held.ID) {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	requireMarkers(t, service.Store)
}

// The hook never reads a message file without a marker. Every closed file
// here is unreadable: a full scan would report them, the hook does not.
func TestHookReadsOnlyMarkedMessages(t *testing.T) {
	f := newOpenFixture(t)
	for range 50 {
		msg := f.closed(t)
		if err := os.WriteFile(filepath.Join(f.store.Dir, "messages", msg.ID+".json"), []byte("not json"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pending := f.queue(t, "the only open message", time.Now())
	output := capture(&f.store)
	text, err := f.store.EmitPending(f.target.ID)
	if err != nil || !strings.Contains(text, pending.ID) {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	if records := skippedRecords(t, output); len(records) != 0 {
		t.Fatalf("the hook read unmarked files: %v", records)
	}
	// The list still scans every file and counts them (B3b).
	batch, err := f.store.ReadMessages(MessageQuery{Actor: f.target.ID, Capability: f.targetKey, History: true})
	if err != nil || batch.Unreadable != 50 {
		t.Fatalf("history unreadable = %d, %v; want 50", batch.Unreadable, err)
	}
}

// A marker whose record is closed or missing (a crash after the marker, before
// the record) is removed by the hook's own read; the record is not changed.
func TestHookRemovesStaleMarkers(t *testing.T) {
	f := newOpenFixture(t)
	closed := f.closed(t)
	missing := newID()
	markOpen(t, f.store, closed.ID)
	markOpen(t, f.store, missing)
	before, err := os.ReadFile(filepath.Join(f.store.Dir, "messages", closed.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if text, err := f.store.EmitPending(f.target.ID); err != nil || text != "" {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	requireMarkers(t, f.store)
	after, err := os.ReadFile(filepath.Join(f.store.Dir, "messages", closed.ID+".json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("removing a stale marker changed the record", err)
	}
	if _, err := os.Lstat(filepath.Join(f.store.Dir, "messages", missing+".json")); !os.IsNotExist(err) {
		t.Fatal("removing a stale marker created a record", err)
	}
}

// The marker is durable before the record: a failed write of an open record
// leaves only an extra marker.
func TestOpenMarkerIsWrittenBeforeTheRecord(t *testing.T) {
	store := fixtureStore(t)
	msg := model.Message{ID: newID(), From: "codex:" + newID(), To: "cursor:" + newID(), Body: "fixture", Status: model.StatusSubmitted}
	nearLimitMessage(t, store, &msg)
	msg.Status = model.StatusPendingActivity
	requireEncodedSizeError(t, store.saveMessage(msg))
	requireMarkers(t, store, msg.ID)
}

// The manual prune restores a lost marker for a hold.
func TestPruneRestoresAHoldMarker(t *testing.T) {
	service, _, held := heldFixture(t)
	loseMarkers(t, service.Store)
	if _, err := service.Store.Prune(false, nil); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, service.Store, held.ID)
}

// The manual prune marks an unreadable file. The marker does not hide it from
// the list count (B3b), and the hook still counts it until it can be read.
func TestPruneMarksAnUnreadableFileAndKeepsEveryCount(t *testing.T) {
	f := newOpenFixture(t)
	bad := newUnreadableMessage(t, f.store, truncatedMessage)
	pending := f.queue(t, "pending", time.Now())
	loseMarkers(t, f.store)
	if _, err := f.store.Prune(false, nil); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, f.store, pending.ID, bad)
	batch, err := f.store.ReadMessages(MessageQuery{Actor: f.target.ID, Capability: f.targetKey})
	if err != nil || batch.Unreadable != 1 || len(batch.Messages) != 1 {
		t.Fatalf("inbox = %d messages, unreadable %d, %v", len(batch.Messages), batch.Unreadable, err)
	}
	output := capture(&f.store)
	if text := emitSkipping(t, f.emitFixture, output, bad); !strings.Contains(text, pending.ID) {
		t.Fatalf("EmitPending = %q", text)
	}
	requireMarkers(t, f.store, bad)
}

// The marker directory has the same private-directory rule as the other
// state directories.
func TestOpenMarkerDirectoryMustBePrivate(t *testing.T) {
	f := newOpenFixture(t)
	dir := filepath.Join(f.store.Dir, openMessagesDir)
	info, err := os.Lstat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("marker directory is not 0700", err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EmitPending(f.target.ID); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatal("a shared marker directory was accepted", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EmitPending(f.target.ID); err == nil || !strings.Contains(err.Error(), "private directory") {
		t.Fatal("a symlinked marker directory was accepted", err)
	}
}

// A store used without the journal lock gets the same check when the first
// marker is written.
func TestOpenMarkerCreatesAPrivateDirectory(t *testing.T) {
	store := fixtureStore(t)
	if err := os.MkdirAll(store.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := newID()
	if err := markers(store).add(id, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(store.Dir, openMessagesDir))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("marker directory is not 0700", err)
	}
}

// The manual zalki prune repairs the markers; a dry run changes none.
func TestPruneRepairsOpenMarkers(t *testing.T) {
	f := newOpenFixture(t)
	pending := f.queue(t, "pending", time.Now())
	closed := f.closed(t)
	if err := os.Remove(markers(f.store).path(pending.ID)); err != nil {
		t.Fatal(err)
	}
	markOpen(t, f.store, closed.ID)
	if _, err := f.store.Prune(true, nil); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, f.store, closed.ID)
	if _, err := f.store.Prune(false, nil); err != nil {
		t.Fatal(err)
	}
	requireMarkers(t, f.store, pending.ID)
}

// stubSync records the directories synced for markers. fail makes the next
// sync fail once.
func stubSync(t *testing.T) (synced *[]string, fail *bool) {
	t.Helper()
	synced, fail = &[]string{}, new(bool)
	syncOpenMarkers = func(dir string) error {
		*synced = append(*synced, dir)
		if *fail {
			*fail = false
			return errors.New("sync failed")
		}
		return nil
	}
	t.Cleanup(func() { syncOpenMarkers = journal.SyncDir })
	return synced, fail
}

// Every open write syncs the marker directory and the state directory in the
// same call, also when the marker already exists. A closed write syncs nothing
// for markers.
func TestEveryOpenWriteSyncsItsMarker(t *testing.T) {
	f := newOpenFixture(t)
	synced, _ := stubSync(t)
	dir := filepath.Join(f.store.Dir, openMessagesDir)
	msg := f.queue(t, "pending", time.Now())
	*synced = nil
	f.rewrite(t, msg.ID, func(m *model.Message) { m.Status = model.StatusWakeWritten })
	if !slices.Equal(*synced, []string{dir, f.store.Dir}) {
		t.Fatalf("marker syncs = %v, want %s then %s", *synced, dir, f.store.Dir)
	}
	*synced = nil
	f.closed(t)
	if len(*synced) != 0 {
		t.Fatalf("a closed write synced markers: %v", *synced)
	}
}

// Another writer made the marker directory and crashed before any sync. The
// next open write still syncs the directory's name before its record.
func TestOpenWriteSyncsADirectoryAnotherWriterMade(t *testing.T) {
	f := newOpenFixture(t)
	dir := filepath.Join(f.store.Dir, openMessagesDir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	msg := f.record(model.StatusPendingActivity)
	var synced []string
	syncOpenMarkers = func(path string) error {
		if current, err := f.store.message(msg.ID); err == nil && openMessage(current) {
			t.Fatal("the open record was written before the syncs")
		}
		synced = append(synced, path)
		return nil
	}
	t.Cleanup(func() { syncOpenMarkers = journal.SyncDir })
	if err := f.store.locked(func() error { return f.store.saveMessage(msg) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(synced, []string{dir, f.store.Dir}) {
		t.Fatalf("syncs = %v, want %s then %s", synced, dir, f.store.Dir)
	}
}

// Two writers: while the first one is stalled in its state-directory sync,
// the second one does not trust the first one's work. It syncs both
// directories itself before its record is written.
func TestASecondWriterDoesNotTrustAStalledSync(t *testing.T) {
	f := newOpenFixture(t)
	first, second := f.record(model.StatusPendingActivity), f.record(model.StatusPendingActivity)
	var mu sync.Mutex
	stateSyncs := 0
	stalled, release := make(chan struct{}), make(chan struct{})
	syncOpenMarkers = func(path string) error {
		if path != f.store.Dir {
			return nil
		}
		mu.Lock()
		stateSyncs++
		n := stateSyncs
		mu.Unlock()
		if n == 1 {
			close(stalled)
			<-release
		}
		return nil
	}
	t.Cleanup(func() { syncOpenMarkers = journal.SyncDir })
	done := make(chan error, 1)
	go func() { done <- f.store.saveMessage(first) }()
	select {
	case <-stalled:
	case <-time.After(5 * time.Second):
		t.Fatal("the first writer never synced the state directory")
	}
	if err := f.store.saveMessage(second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	n := stateSyncs
	mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("the second writer made %d state-directory syncs before its record, want 1", n-1)
	}
}

// A failed sync leaves no marker and no record. A marker that already exists
// (left by another process, or kept when its removal failed too) is synced
// again before the record is written.
func TestOpenMarkerIsSyncedAgainAfterAFailure(t *testing.T) {
	f := newOpenFixture(t)
	synced, fail := stubSync(t)
	*fail = true
	msg := f.record(model.StatusPendingActivity)
	if err := f.store.saveMessage(msg); err == nil {
		t.Fatal("the save ignored a failed marker sync")
	}
	requireMarkers(t, f.store)
	if _, err := os.Lstat(filepath.Join(f.store.Dir, "messages", msg.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("the record was written after the marker sync failed", err)
	}
	markOpen(t, f.store, msg.ID)
	*synced = nil
	if err := f.store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*synced, []string{filepath.Join(f.store.Dir, openMessagesDir), f.store.Dir}) {
		t.Fatalf("an existing marker was trusted without a sync: syncs %v", *synced)
	}
}

// The hook's approval scan also reads only marked files: unmarked unreadable
// files give no diagnostic, and the hold is still released.
func TestHookApprovalScanReadsOnlyMarkedMessages(t *testing.T) {
	service, target, held := heldFixture(t)
	for range 3 {
		if err := os.WriteFile(filepath.Join(service.Store.Dir, "messages", newID()+".json"), []byte("not json"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := capture(&service.Store)
	text, err := service.Store.EmitPending(target.ID)
	if err != nil || !strings.Contains(text, held.ID) {
		t.Fatalf("EmitPending = %q, %v", text, err)
	}
	if records := skippedRecords(t, output); len(records) != 0 {
		t.Fatalf("the approval scan read unmarked files: %v", records)
	}
	if got := approvalHeld(t, service.Store, target.ID); got != 0 {
		t.Fatalf("held count = %d", got)
	}
}

// Writer A fails its state-directory sync and leaves the marker directory. A
// later writer syncs the state directory itself before its open record.
func TestALaterWriterSyncsTheStateDirAfterAFailedSync(t *testing.T) {
	f := newOpenFixture(t)
	first, second := f.record(model.StatusPendingActivity), f.record(model.StatusPendingActivity)
	var synced []string
	failed := false
	syncOpenMarkers = func(dir string) error {
		synced = append(synced, dir)
		if dir == f.store.Dir && !failed {
			failed = true
			return errors.New("state directory sync failed")
		}
		return nil
	}
	t.Cleanup(func() { syncOpenMarkers = journal.SyncDir })
	if err := f.store.saveMessage(first); err == nil {
		t.Fatal("the save ignored a failed state directory sync")
	}
	if _, err := os.Lstat(filepath.Join(f.store.Dir, openMessagesDir)); err != nil {
		t.Fatal(err)
	}
	synced = nil
	if err := f.store.saveMessage(second); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(synced, f.store.Dir) {
		t.Fatalf("the later writer trusted the directory: syncs %v", synced)
	}
}

// Prune repairs the markers before it settles approvals, so a held message to
// a retired target is settled even when its marker was lost.
func TestPruneSettlesAHoldWhoseMarkerWasLost(t *testing.T) {
	service, target, held := heldFixture(t)
	loseMarkers(t, service.Store)
	if _, err := service.Store.Prune(false, []string{target.ID}); err != nil {
		t.Fatal(err)
	}
	msg, err := service.Store.message(held.ID)
	if err != nil || msg.IsQueuedForApproval() {
		t.Fatalf("the held message was not settled: %+v %v", msg, err)
	}
	requireMarkers(t, service.Store)
}

// closeWithFailedSync writes msg as closed while messages/ cannot be opened for
// its sync: the rename succeeds, so the closed record is visible, but its
// directory sync fails (EACCES), so the record may not be durable yet.
func closeWithFailedSync(t *testing.T, f openFixture, msg model.Message) {
	t.Helper()
	dir := filepath.Join(f.store.Dir, "messages")
	if err := os.Chmod(dir, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	msg.AcknowledgedAt = model.Timestamp()
	if err := f.store.saveMessage(msg); err == nil {
		t.Fatal("the closing write did not report the failed directory sync")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if current, err := f.store.message(msg.ID); err != nil || openMessage(current) {
		t.Fatalf("the closed record is not visible: %+v %v", current, err)
	}
	requireMarkers(t, f.store, msg.ID)
}

// A scan that finds a stale marker syncs messages/ before it removes the
// marker, so the removal can never be durable before the closed record. If
// that sync fails, the marker stays.
func TestAStaleMarkerIsRemovedOnlyAfterMessagesAreSynced(t *testing.T) {
	for _, scan := range []string{"hook", "prune"} {
		t.Run(scan, func(t *testing.T) { checkStaleMarkerOrder(t, scan) })
	}
}

func checkStaleMarkerOrder(t *testing.T, scan string) {
	t.Helper()
	f := newOpenFixture(t)
	msg := f.queue(t, "pending", time.Now())
	closeWithFailedSync(t, f, msg)
	synced, fail := stubMessagesSync(t, f, msg.ID)
	runStaleScan(t, f, scan)
	requireMarkers(t, f.store, msg.ID)
	*fail = false
	runStaleScan(t, f, scan)
	requireMarkers(t, f.store)
	if *synced != 2 {
		t.Fatalf("messages/ syncs = %d, want one per scan that found the stale marker", *synced)
	}
}

// stubMessagesSync counts messages/ syncs, checks that the marker for id still
// exists at each one, and fails them while *fail is true (it starts true).
func stubMessagesSync(t *testing.T, f openFixture, id string) (synced *int, fail *bool) {
	t.Helper()
	messages := filepath.Join(f.store.Dir, "messages")
	synced, fail = new(int), new(bool)
	*fail = true
	syncOpenMarkers = func(dir string) error {
		if dir != messages {
			return nil
		}
		*synced++
		if _, err := os.Lstat(markers(f.store).path(id)); err != nil {
			t.Fatal("the marker was removed before messages/ was synced")
		}
		if *fail {
			return errors.New("messages sync failed")
		}
		return nil
	}
	t.Cleanup(func() { syncOpenMarkers = journal.SyncDir })
	return synced, fail
}

// runStaleScan runs the hook scan or the prune repair once.
func runStaleScan(t *testing.T, f openFixture, scan string) {
	t.Helper()
	var err error
	if scan == "hook" {
		_, err = f.store.EmitPending(f.target.ID)
	} else {
		_, err = f.store.Prune(false, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
}
