package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOrderedTargetBarrierPrecedesRename(t *testing.T) {
	f := newBatchFixture(t)
	put := f.batch.Puts[0]
	path := filepath.Join(f.log.Root, put.Path)
	err := writeBatchTargetWith(path, put.Data, func(file *os.File) error {
		if got := readBatchBytes(t, path); !bytes.Equal(got, f.old) {
			t.Fatal("target renamed before its file barrier")
		}
		if info, err := file.Stat(); err != nil || info.Size() != int64(len(put.Data)) {
			t.Fatal("temp data not written before file barrier", err)
		}
		return errBatchCut
	})
	if !errors.Is(err, errBatchCut) {
		t.Fatal(err)
	}
	assertBatchTargets(t, f, false)
}

func TestBatchOrdersUniqueDirectoriesBeforeUnlink(t *testing.T) {
	f := retainingFixture(t)
	ops := f.log.operations()
	ordered := map[string]bool{}
	removedIntent := false
	ops.target = writeBatchTarget
	ops.order = func(path string) error {
		if ordered[path] {
			t.Fatal("duplicate parent barrier", path)
		}
		ordered[path] = true
		return orderBatchDirectory(path)
	}
	ops.remove = func(path string) error {
		if path == f.log.intentPath() {
			if len(ordered) != 2 {
				t.Fatal("intent removed before both target directories were ordered")
			}
			removedIntent = true
		}
		return os.Remove(path)
	}
	ops.sync = func(path string) error {
		if !removedIntent {
			t.Fatal("fresh commit redundantly resynced its already durable intent")
		}
		return SyncDir(path)
	}
	f.log.io = &ops
	if err := f.log.Commit(f.batch); err != nil {
		t.Fatal(err)
	}
	assertBatchTargets(t, f, true)
}

func TestBatchBarrierCutsKeepReplayableIntent(t *testing.T) {
	for cut := range 5 {
		t.Run(string(rune('0'+cut)), func(t *testing.T) {
			assertBatchBarrierCut(t, cut)
		})
	}
}

func assertBatchBarrierCut(t *testing.T, cut int) {
	t.Helper()
	f := retainingFixture(t)
	ops := f.log.operations()
	step := 0
	barrier := func() error {
		current := step
		step++
		if current == cut {
			return errBatchCut
		}
		return nil
	}
	ops.target = func(path string, raw []byte) error {
		return writeBatchTargetWith(path, raw, func(file *os.File) error {
			if err := barrier(); err != nil {
				return err
			}
			return orderBatchFile(file)
		})
	}
	ops.order = func(path string) error {
		if err := barrier(); err != nil {
			return err
		}
		return orderBatchDirectory(path)
	}
	f.log.io = &ops
	if err := f.log.Commit(f.batch); !errors.Is(err, errBatchCut) {
		t.Fatalf("cut %d: %v", cut, err)
	}
	if _, err := os.Stat(f.log.intentPath()); err != nil {
		t.Fatal("barrier failure removed the intent", err)
	}
	f.log.io = nil
	if err := f.log.Recover(nil); err != nil {
		t.Fatal(err)
	}
	assertBatchTargets(t, f, true)
}
