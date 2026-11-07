package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func retainingFixture(t *testing.T) batchFixture {
	t.Helper()
	f := newBatchFixture(t)
	oldest := []byte("retired event\n")
	if err := os.WriteFile(filepath.Join(f.log.Root, "outbox/old.json"), oldest, 0600); err != nil {
		t.Fatal(err)
	}
	f.batch.Puts = append(f.batch.Puts, NewPut("state/meta.json", nil, []byte("{\"first\":2}\n")))
	f.batch.Deletes = []Delete{NewDelete("outbox/old.json", oldest)}
	return f
}

type retentionFault struct {
	log     BatchLog
	cut     string
	deleted bool
	removed bool
}

func retentionCutIO(log BatchLog, cut string) *batchIO {
	fault := &retentionFault{log: log, cut: cut}
	return &batchIO{write: fault.write, remove: fault.remove, sync: fault.sync}
}

func (f *retentionFault) write(path string, raw []byte) error {
	if err := WriteAtomic(path, raw); err != nil {
		return err
	}
	if f.cut == "after-meta" && filepath.Base(path) == "meta.json" {
		return errBatchCut
	}
	return nil
}

func (f *retentionFault) remove(path string) error {
	intent := path == f.log.intentPath()
	before, after := "before-delete", "after-delete"
	if intent {
		before, after = "before-intent-remove", "after-intent-remove"
	}
	if f.cut == before {
		return errBatchCut
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	f.deleted = f.deleted || !intent
	f.removed = f.removed || intent
	if f.cut == after {
		return errBatchCut
	}
	return nil
}

func (f *retentionFault) sync(path string) error {
	if f.deleted && !f.removed && f.cut == "delete-sync" {
		return errBatchCut
	}
	if f.removed && f.cut == "intent-remove-sync" {
		return errBatchCut
	}
	return SyncDir(path)
}

func TestBatchAtomicRetentionCrashCuts(t *testing.T) {
	for _, cut := range []string{"after-meta", "before-delete", "after-delete", "delete-sync", "before-intent-remove", "after-intent-remove", "intent-remove-sync"} {
		t.Run(cut, func(t *testing.T) {
			f := retainingFixture(t)
			interrupted := f.log
			interrupted.io = retentionCutIO(f.log, cut)
			if err := interrupted.Commit(f.batch); !errors.Is(err, errBatchCut) {
				t.Fatalf("cut error = %v", err)
			}
			if err := f.log.Recover(nil); err != nil {
				t.Fatal(err)
			}
			assertBatchTargets(t, f, true)
			if _, err := os.Stat(filepath.Join(f.log.Root, "outbox/old.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old event remains: %v", err)
			}
			// A removal whose directory sync failed can reappear after restart.
			writeBatchIntent(t, f)
			if err := f.log.Recover(nil); err != nil {
				t.Fatalf("repeat after removal: %v", err)
			}
		})
	}
}

func TestBatchDeleteConflictPrecedesEveryPut(t *testing.T) {
	f := retainingFixture(t)
	if err := os.WriteFile(filepath.Join(f.log.Root, "outbox/old.json"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.log.Commit(f.batch); !errors.Is(err, ErrBatchConflict) {
		t.Fatalf("changed delete target: %v", err)
	}
	assertBatchTargets(t, f, false)
}

func TestBatchRejectsInvalidDeletes(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Batch)
	}{
		{"escape", func(b *Batch) { b.Deletes[0].Path = "../escape" }},
		{"checksum", func(b *Batch) { b.Deletes[0].PreviousSHA256 = "" }},
		{"overlap", func(b *Batch) { b.Deletes[0].Path = b.Puts[0].Path }},
		{"duplicate", func(b *Batch) { b.Deletes = append(b.Deletes, b.Deletes[0]) }},
		{"intent", func(b *Batch) { b.Deletes[0].Path = "intents/pending.json" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := retainingFixture(t)
			test.change(&f.batch)
			if err := f.log.Commit(f.batch); !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("invalid delete: %v", err)
			}
			assertBatchTargets(t, f, false)
		})
	}
}

func TestBatchCombinedOperationLimit(t *testing.T) {
	f := retainingFixture(t)
	for i := len(f.batch.Puts); i < maxBatchPuts; i++ {
		f.batch.Puts = append(f.batch.Puts, NewPut(fmtBatchPath(i), nil, nil))
	}
	if _, err := f.batch.encode(); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("combined limit = %v", err)
	}
}

func TestBatchNeverDeletesSymlink(t *testing.T) {
	f := retainingFixture(t)
	path := filepath.Join(f.log.Root, "outbox/old.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.log.Root, "state/one.json"), path); err != nil {
		t.Fatal(err)
	}
	if err := f.log.Commit(f.batch); !errors.Is(err, ErrInvalidFile) {
		t.Fatalf("symlink delete: %v", err)
	}
	assertBatchTargets(t, f, false)
}
