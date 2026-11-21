package bus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
)

func TestStoreArchiveWaiterProcessHelper(t *testing.T) {
	root := os.Getenv("ZALKI_TEST_ARCHIVE_WAITER")
	if root == "" {
		t.Skip("private process helper")
	}
	ready := os.NewFile(3, "opened-old-lock")
	gate := os.NewFile(4, "continue-to-flock")
	defer ready.Close() // Test-owned pipe.
	defer gate.Close()  // Test-owned pipe.
	store := fixtureStore(t)
	store.dir = root
	opens := 0
	storeLockOpened = func() {
		opens++
		if _, err := ready.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		var b [1]byte
		if _, err := io.ReadFull(gate, b[:]); err != nil {
			t.Fatal(err)
		}
	}
	err := store.locked(func() error {
		return os.WriteFile(filepath.Join(root, "unexpected-callback"), []byte("entered"), 0600)
	})
	if errors.Is(err, errJournalRootChanged) && opens == 1 {
		fmt.Println("STATE_CHANGED_REFUSAL_ONE_OPEN")
	} else {
		fmt.Printf("WAITER_OTHER_RESULT opens=%d err=%v\n", opens, err)
	}
}

func TestStoreArchiveWaiterRefusesBeforeFreshRecovery(t *testing.T) {
	store := fixtureStore(t)
	if err := store.locked(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	oldFD, err := journal.OpenLockFile(filepath.Join(store.dir, "lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	oldOpen := true
	defer func() {
		if oldOpen {
			_ = journal.CloseLockFile(oldFD)
		}
	}()
	if err := journal.LockExclusive(oldFD, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, "retained-sentinel"), []byte("old-tree-opaque-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	waiter := startArchiveWaiter(t, store.dir)
	archiveBefore := archiveWaiterFiles(t, store.dir)
	archive := store.dir + "-archived"
	if err := os.Rename(store.dir, archive); err != nil {
		t.Fatal(err)
	}
	fresh := stageFreshArchiveState(t, store.dir)
	pending := filepath.Join(store.dir, "commits", "pending.json")
	before := readCommitFile(t, pending)
	priorSubject := readCommitFile(t, filepath.Join(store.dir, eventSubjectPath(fresh.event)))
	if _, err := waiter.gate.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := journal.CloseLockFile(oldFD); err != nil {
		t.Fatal(err)
	}
	oldOpen = false
	waitErr := <-waiter.done
	assertArchiveWaiterOutcome(t, waiter, waitErr, store.dir, archive, fresh, pending, before, priorSubject)
	if !reflect.DeepEqual(archiveBefore, archiveWaiterFiles(t, archive)) {
		t.Fatal("STALE_LOCK_MODIFIED_ARCHIVED_TREE")
	}
}

type archiveWaiter struct {
	gate   *os.File
	done   <-chan error
	output *bytes.Buffer
}

func startArchiveWaiter(t *testing.T, root string) archiveWaiter {
	t.Helper()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()  // Read completed before returning.
	defer readyWrite.Close() // Parent copy is closed after start.
	gateRead, gateWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gateRead.Close() // Parent copy is closed after start.
	t.Cleanup(func() { _ = gateWrite.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreArchiveWaiterProcessHelper$")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "TMPDIR=" + filepath.Dir(root), "GOTELEMETRY=off", "ZALKI_TEST_ARCHIVE_WAITER=" + root}
	cmd.ExtraFiles = []*os.File{readyWrite, gateRead}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyWrite.Close()
	_ = gateRead.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }() // Sole Wait owner, including failure cleanup.
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	if err := readyRead.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(readyRead, b[:]); err != nil {
		t.Fatal("waiter never opened the old lock", err)
	}
	return archiveWaiter{gate: gateWrite, done: done, output: &output}
}

func stageFreshArchiveState(t *testing.T, root string) commitFixture {
	t.Helper()
	fresh := newCommitFixture(t)
	if err := os.Rename(fresh.writer.dir, root); err != nil {
		t.Fatal(err)
	}
	fresh.writer.dir = root
	for _, name := range []string{"messages", openMessagesDir} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	newFD, err := journal.OpenLockFile(filepath.Join(root, "lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.CloseLockFile(newFD) }) // Hold C through B exit, including setup failures.
	if err := journal.LockExclusive(newFD, true); err != nil {
		t.Fatal(err)
	}
	stageCommit(t, fresh, preparedCommit(t, fresh))
	return fresh
}

func assertArchiveWaiterOutcome(t *testing.T, waiter archiveWaiter, waitErr error, root, archive string, fresh commitFixture, pending string, before, priorSubject []byte) {
	t.Helper()
	after, readErr := os.ReadFile(pending)
	subject, subjectErr := os.ReadFile(filepath.Join(root, eventSubjectPath(fresh.event)))
	if readErr != nil || subjectErr != nil || !bytes.Equal(before, after) || !bytes.Equal(priorSubject, subject) {
		t.Fatalf("STALE_LOCK_ENTERED_NEW_RECOVERY: pending=%v subject=%v output=%s", readErr, subjectErr, waiter.output.String())
	}
	if waitErr != nil || !bytes.Contains(waiter.output.Bytes(), []byte("STATE_CHANGED_REFUSAL_ONE_OPEN")) {
		t.Fatalf("STALE_LOCK_NOT_NAMED_REFUSAL: %v %s", waitErr, waiter.output.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "unexpected-callback")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("STALE_LOCK_ENTERED_CALLBACK", err)
	}
	if _, err := os.Lstat(filepath.Join(archive, "unexpected-callback")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("STALE_LOCK_WROTE_ARCHIVE", err)
	}
}

type archiveWaiterImage struct {
	raw   string
	mode  os.FileMode
	mtime int64
}

func archiveWaiterFiles(t *testing.T, root string) map[string]archiveWaiterImage {
	t.Helper()
	images := map[string]archiveWaiterImage{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, readErr error) error {
		if readErr != nil {
			return readErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		var raw []byte
		if info.Mode().IsRegular() {
			raw, err = os.ReadFile(path)
		}
		images[relative] = archiveWaiterImage{raw: string(raw), mode: info.Mode(), mtime: info.ModTime().UnixNano()}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return images
}
