package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestRetiredRetentionBoundaryIsStrictlyOlder(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	window := 7 * 24 * time.Hour
	exact := now.Add(-window).Format(time.RFC3339Nano)
	older := now.Add(-window - time.Nanosecond).Format(time.RFC3339Nano)
	if model.RetiredLongEnough(exact, now, window) {
		t.Fatal("a record at the retention boundary was eligible")
	}
	if !model.RetiredLongEnough(older, now, window) {
		t.Fatal("a record older than the window was kept")
	}
	if model.RetiredLongEnough("not-a-time", now, window) {
		t.Fatal("an unparsable timestamp was eligible")
	}
}

func TestPruneDeletesOnlyOldRetiredSessions(t *testing.T) {
	store := fixtureStore(t)
	from, fromKey, to, toKey := registerPair(t, store, "codex", "cursor")
	live, _, young, _ := registerPair(t, store, "cursor", "cursor")
	service := &Service{Store: store}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: fromKey, To: to.ID, MessageID: newID(), Body: "keep this history"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	from.Retired, from.State, from.RetiredReason, from.UpdatedAt = true, StateRetired, "session_end", old
	live.UpdatedAt = old
	young.Retired, young.State, young.RetiredReason = true, StateRetired, "session_end"
	for _, v := range []Session{from, live, young} {
		if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
			t.Fatal(err)
		}
	}
	freeLock := store.listenerLockPath(from.Kind, from.ID)
	if err := os.WriteFile(freeLock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	heldSession, _ := register(t, store, "cursor")
	heldSession.Retired, heldSession.State, heldSession.RetiredReason, heldSession.UpdatedAt = true, StateRetired, "session_end", old
	if err := store.locked(func() error { return store.saveSession(heldSession) }); err != nil {
		t.Fatal(err)
	}
	heldLock := store.listenerLockPath(heldSession.Kind, heldSession.ID)
	fd, err := syscall.Open(heldLock, syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)

	preview, err := store.PruneRetired(true, nil, DefaultRetiredRetention)
	if err != nil || len(preview.DeletedSessions) != 2 || preview.DeletedSessions[0].Reason != "retention" {
		t.Fatalf("dry-run did not report expired sessions: %+v %v", preview, err)
	}
	if _, err := os.Stat(store.sessionPathMust(t, from.ID)); err != nil {
		t.Fatal("dry-run removed a session", err)
	}
	if _, err := os.Stat(freeLock); err != nil {
		t.Fatal("dry-run removed a lock", err)
	}

	report, err := store.PruneRetired(false, nil, DefaultRetiredRetention)
	if err != nil || len(report.DeletedSessions) != 2 {
		t.Fatalf("retention delete failed: %+v %v", report, err)
	}
	if _, err := os.Stat(store.sessionPathMust(t, from.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old retired session remained", err)
	}
	if _, err := os.Stat(freeLock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("free listener lock remained", err)
	}
	if _, err := os.Stat(store.sessionPathMust(t, heldSession.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old retired session with a held lock remained", err)
	}
	if _, err := os.Stat(heldLock); err != nil {
		t.Fatal("removed a listener lock that was still held", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	unlocked, err := store.Prune(false, nil)
	if err != nil || len(unlocked.DeletedLocks) != 1 || unlocked.DeletedLocks[0] != filepath.Base(heldLock) {
		t.Fatalf("free orphan lock was not removed: %+v %v", unlocked, err)
	}
	if _, err := os.Stat(heldLock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unlocked listener lock remained", err)
	}
	for _, id := range []string{to.ID, live.ID, young.ID} {
		if _, err := os.Stat(store.sessionPathMust(t, id)); err != nil {
			t.Fatalf("removed %s: %v", id, err)
		}
	}
	messagePath := filepath.Join(store.Dir, "messages", msg.ID+".json")
	if _, err := os.Stat(messagePath); err != nil {
		t.Fatal("deleted message history", err)
	}
	history, err := store.ReadMessages(MessageQuery{Actor: to.ID, Capability: toKey, History: true, Limit: 10, IncludeBody: true})
	if err != nil || len(history.Messages) != 1 || history.Messages[0].Body != "keep this history" || history.Messages[0].From != from.ID || history.Messages[0].FromLabel != "Codex" {
		t.Fatalf("recipient could not read the deleted sender's message: %+v %v", history.Messages, err)
	}
	reply, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: toKey, MessageID: msg.ID, Body: "still here"})
	if err != nil || reply.To != from.ID || reply.Status != StatusNotSent || reply.FromLabel != "Cursor" {
		t.Fatalf("reply did not survive a deleted sender record: %+v %v", reply, err)
	}
	if _, err := store.PruneRetired(false, nil, -time.Second); err == nil {
		t.Fatal("accepted a negative retention window")
	}
}

func TestPruneRemovesAnUnlockedOrphanListenerLock(t *testing.T) {
	store := fixtureStore(t)
	live, _ := register(t, store, "cursor")
	orphan := store.listenerLockPath("cursor", "cursor:"+newID())
	liveLock := store.listenerLockPath(live.Kind, live.ID)
	for _, path := range []string{orphan, liveLock} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.Prune(false, nil)
	if err != nil || len(report.DeletedSessions) != 0 || len(report.DeletedLocks) != 1 || report.DeletedLocks[0] != filepath.Base(orphan) {
		t.Fatalf("orphan lock cleanup: %+v %v", report, err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan lock remained", err)
	}
	if _, err := os.Stat(liveLock); err != nil {
		t.Fatal("removed a live session's listener lock", err)
	}
	if _, err := os.Stat(store.sessionPathMust(t, live.ID)); err != nil {
		t.Fatal("removed a live session", err)
	}
}

func (s Store) sessionPathMust(t *testing.T, id string) string {
	t.Helper()
	path, err := s.sessionPath(id)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
