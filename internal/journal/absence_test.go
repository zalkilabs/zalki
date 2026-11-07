package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBatchAbsentRecoveryPreventsStaleIntentResurrection(t *testing.T) {
	f := newBatchFixture(t)
	interrupted := f.log
	interrupted.io = batchCutIO(f.log, "final-sync")
	if err := interrupted.Commit(f.batch); !errors.Is(err, errBatchCut) {
		t.Fatal(err)
	}
	// Model directory durability: the removed entry can still return until a
	// later successful sync. Its live pathname is already absent at this point.
	canResurrect := true
	ops := f.log.operations()
	ops.sync = func(path string) error {
		if err := SyncDir(path); err != nil {
			return err
		}
		if path == filepath.Dir(f.log.intentPath()) {
			canResurrect = false
		}
		return nil
	}
	f.log.io = &ops
	if err := f.log.Recover(nil); err != nil {
		t.Fatal(err)
	}
	// A legacy same-state Update or reply can rewrite the bytes after recovery.
	legacy := []byte("{\"value\":\"new\",\"updated_at\":\"later\"}\n")
	if err := WriteAtomic(filepath.Join(f.log.Root, f.batch.Puts[0].Path), legacy); err != nil {
		t.Fatal(err)
	}
	if canResurrect {
		writeBatchIntent(t, f)
	}
	f.log.io = nil
	if err := f.log.Recover(nil); err != nil {
		t.Fatalf("stale intent returned after legacy rewrite: %v", err)
	}
	if canResurrect {
		t.Fatal("missing-intent recovery did not establish durable absence")
	}
}

func TestBatchAbsentRecoveryPropagatesDirectorySyncFailure(t *testing.T) {
	f := newBatchFixture(t)
	ops := f.log.operations()
	ops.sync = func(string) error { return errBatchCut }
	f.log.io = &ops
	if err := f.log.Recover(nil); !errors.Is(err, errBatchCut) {
		t.Fatalf("absence sync failure = %v", err)
	}
	if _, err := os.Stat(f.log.intentPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absence barrier created an intent")
	}
}
