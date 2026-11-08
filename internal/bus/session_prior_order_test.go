package bus

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// This models the permitted durable namespace after a rename whose directory
// sync failed. The filesystem cut is independent of the ordering callback.
func TestZeroEventPriorRenameOrdering(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires an ordinary user")
	}
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	path, _ := store.sessionPath(session.ID)
	directory := filepath.Dir(path)
	durable := readCommitFile(t, path) // D0: ready, previous private data.
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
	session.Task = "D1 private value"
	simulatePrivateRenameSyncFailure(t, store, session, path, directory)
	d1 := readCommitFile(t, path)
	if bytes.Equal(durable, d1) {
		t.Fatal("private rename never published D1")
	}
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	writer := commitWriter{dir: store.Dir, orderSession: func(parent string) error {
		if parent != directory {
			t.Fatal("ordered another directory", parent)
		}
		if _, err := os.Stat(filepath.Join(store.Dir, "commits", "pending.json")); !os.IsNotExist(err) {
			t.Fatal("intent published before prior ordering", err)
		}
		if err := journal.OrderDirectory(parent); err != nil {
			return err
		}
		durable = bytes.Clone(d1) // D1 namespace now submitted before FULL intent sync.
		return nil
	}}
	publishEventBeforeTargetCut(t, store, session, writer)
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	// Power loss retains only the namespace submitted before the durable intent.
	if err := journal.WriteAtomic(path, durable); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err != nil {
		t.Fatalf("prior rename not ordered before event intent; replay = %v", err)
	}
	next, err := store.session(session.ID)
	if err != nil || next.State != model.StateBusy || next.Task != "D1 private value" {
		t.Fatalf("D2 replay = %+v,%v", next, err)
	}
	if eventState(t, store).Count != 2 {
		t.Fatal("private save emitted a fact or event replay duplicated")
	}
}

func simulatePrivateRenameSyncFailure(t *testing.T, store Store, session model.Session, path, directory string) {
	t.Helper()
	if err := os.Chmod(directory, 0300); err != nil {
		t.Fatal(err)
	}
	err := store.locked(func() error { return store.saveSession(session) })
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("private rename sync cut = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "commits", "pending.json")); !os.IsNotExist(err) {
		t.Fatal("zero-event save published an intent", err)
	}
	got, err := store.session(session.ID)
	if err != nil || got.Task != session.Task {
		t.Fatalf("rename failed before the directory-sync cut: %+v,%v", got, err)
	}
	if len(readCommitFile(t, path)) == 0 {
		t.Fatal("private after-image missing")
	}
}

func publishEventBeforeTargetCut(t *testing.T, store Store, session model.Session, writer commitWriter) {
	t.Helper()
	session.State = model.StateBusy
	err := store.locked(func() error { return saveSessionWith(store, session, writer) })
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("event target write cut = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "commits", "pending.json")); err != nil {
		t.Fatal("event intent was not durable before target cut", err)
	}
}
