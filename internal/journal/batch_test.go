package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var errBatchCut = errors.New("injected batch interruption")

type batchFixture struct {
	log   BatchLog
	batch Batch
	old   []byte
}

func newBatchFixture(t *testing.T) batchFixture {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"intents", "state", "outbox"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	old := []byte("{\"value\":\"old\"}\n")
	if err := os.WriteFile(filepath.Join(root, "state", "one.json"), old, 0600); err != nil {
		t.Fatal(err)
	}
	return batchFixture{
		log: BatchLog{Root: root, Intent: "intents/pending.json"},
		batch: Batch{ID: "batch-1", Puts: []Put{
			NewPut("state/one.json", old, []byte("{\"value\":\"new\",\"extension\":9}\n")),
			NewPut("outbox/event.json", nil, []byte("{\"id\":\"fixed-event-id\"}\n")),
		}},
		old: old,
	}
}

func batchCutIO(log BatchLog, cut string) *batchIO {
	removed := false
	return &batchIO{
		write: func(path string, data []byte) error {
			stage := "after-" + filepath.Base(filepath.Dir(path))
			if path == log.intentPath() && cut == "before-intent" {
				return errBatchCut
			}
			if err := WriteAtomic(path, data); err != nil {
				return err
			}
			if cut == stage {
				return errBatchCut
			}
			return nil
		},
		remove: func(path string) error {
			if cut == "before-remove" {
				return errBatchCut
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			removed = true
			if cut == "after-remove" {
				return errBatchCut
			}
			return nil
		},
		sync: func(path string) error {
			if removed && cut == "final-sync" {
				return errBatchCut
			}
			return SyncDir(path)
		},
	}
}

func readBatchBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertBatchTargets(t *testing.T, f batchFixture, committed bool) {
	t.Helper()
	want := f.old
	if committed {
		want = f.batch.Puts[0].Data
	}
	if got := readBatchBytes(t, filepath.Join(f.log.Root, f.batch.Puts[0].Path)); !bytes.Equal(got, want) {
		t.Fatalf("state = %q, want %q", got, want)
	}
	path := filepath.Join(f.log.Root, f.batch.Puts[1].Path)
	if !committed {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("uncommitted event exists: %v", err)
		}
		return
	}
	if got := readBatchBytes(t, path); !bytes.Equal(got, f.batch.Puts[1].Data) {
		t.Fatalf("event bytes changed: %q", got)
	}
}

func TestBatchCrashCutsRecoverExactBytes(t *testing.T) {
	for _, cut := range []string{"before-intent", "after-intents", "after-state", "after-outbox", "before-remove", "after-remove", "final-sync"} {
		t.Run(cut, func(t *testing.T) {
			f := newBatchFixture(t)
			interrupted := f.log
			interrupted.io = batchCutIO(f.log, cut)
			if err := interrupted.Commit(f.batch); !errors.Is(err, errBatchCut) {
				t.Fatalf("cut error = %v", err)
			}
			if err := f.log.Recover(nil); err != nil {
				t.Fatal(err)
			}
			assertBatchTargets(t, f, cut != "before-intent")
			if _, err := os.Stat(f.log.intentPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending intent remains: %v", err)
			}
		})
	}
}

func TestBatchRemovalCanReappearAfterSyncFailure(t *testing.T) {
	f := newBatchFixture(t)
	interrupted := f.log
	interrupted.io = batchCutIO(f.log, "final-sync")
	if err := interrupted.Commit(f.batch); !errors.Is(err, errBatchCut) {
		t.Fatal(err)
	}
	// A power loss can restore the directory entry whose removal was not synced.
	writeBatchIntent(t, f)
	if err := f.log.Recover(nil); err != nil {
		t.Fatal(err)
	}
	assertBatchTargets(t, f, true)
}

func writeBatchIntent(t *testing.T, f batchFixture) {
	t.Helper()
	raw, err := f.batch.encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(f.log.intentPath(), raw); err != nil {
		t.Fatal(err)
	}
}

func TestBatchPublishesIntentThenOrderedTargets(t *testing.T) {
	f := newBatchFixture(t)
	var order []string
	ops := f.log.operations()
	ops.write = func(path string, raw []byte) error {
		if path != f.log.intentPath() {
			if _, err := os.Stat(f.log.intentPath()); err != nil {
				t.Fatalf("target published before intent: %v", err)
			}
		}
		relative, err := filepath.Rel(f.log.Root, path)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, relative)
		return WriteAtomic(path, raw)
	}
	f.log.io = &ops
	if err := f.log.Commit(f.batch); err != nil {
		t.Fatal(err)
	}
	want := []string{f.log.Intent, "state/one.json", "outbox/event.json"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("publication order = %v, want %v", order, want)
	}
}

func TestBatchRedoDoesNotRewriteAppliedTargets(t *testing.T) {
	f := newBatchFixture(t)
	if err := f.log.Commit(f.batch); err != nil {
		t.Fatal(err)
	}
	writeBatchIntent(t, f)
	ops := f.log.operations()
	ops.write = func(path string, _ []byte) error {
		t.Fatalf("redo rewrote already applied target %s", path)
		return errBatchCut
	}
	f.log.io = &ops
	if err := f.log.Recover(nil); err != nil {
		t.Fatal(err)
	}
	assertBatchTargets(t, f, true)
}

func TestBatchRecoverySyncsIntentBeforeAnyTarget(t *testing.T) {
	f := newBatchFixture(t)
	writeBatchIntent(t, f)
	ops := f.log.operations()
	ops.sync = func(string) error { return errBatchCut }
	f.log.io = &ops
	if err := f.log.Recover(nil); !errors.Is(err, errBatchCut) {
		t.Fatalf("sync failure = %v", err)
	}
	assertBatchTargets(t, f, false)
}

func TestBatchChecksAllTargetsBeforeRedo(t *testing.T) {
	f := newBatchFixture(t)
	writeBatchIntent(t, f)
	if err := os.WriteFile(filepath.Join(f.log.Root, "outbox/event.json"), []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.log.Recover(nil); !errors.Is(err, ErrBatchConflict) {
		t.Fatalf("conflicting target = %v", err)
	}
	if got := readBatchBytes(t, filepath.Join(f.log.Root, "state/one.json")); !bytes.Equal(got, f.old) {
		t.Fatalf("earlier target changed before a later conflict: %q", got)
	}
}

func TestBatchPendingIntentIsImmutable(t *testing.T) {
	f := newBatchFixture(t)
	writeBatchIntent(t, f)
	original := readBatchBytes(t, f.log.intentPath())
	other := f.batch
	other.ID = "different-batch"
	if err := f.log.Commit(other); !errors.Is(err, ErrBatchConflict) {
		t.Fatalf("replacement = %v", err)
	}
	if got := readBatchBytes(t, f.log.intentPath()); !bytes.Equal(got, original) {
		t.Fatal("pending intent was replaced")
	}
	if err := f.log.Commit(f.batch); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	assertBatchTargets(t, f, true)
}

func TestBatchRejectsInvalidInputBeforePublication(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Batch)
	}{
		{"missing id", func(b *Batch) { b.ID = "" }},
		{"id", func(b *Batch) { b.ID = "../escape" }},
		{"empty", func(b *Batch) { b.Puts = nil }},
		{"escape", func(b *Batch) { b.Puts[0].Path = "../escape" }},
		{"absolute", func(b *Batch) { b.Puts[0].Path = "/escape" }},
		{"noncanonical", func(b *Batch) { b.Puts[0].Path = "state/../escape" }},
		{"duplicate", func(b *Batch) { b.Puts[1].Path = b.Puts[0].Path }},
		{"intent target", func(b *Batch) { b.Puts[0].Path = "intents/pending.json" }},
		{"checksum", func(b *Batch) { b.Puts[0].Data[0] = 'x' }},
		{"prior checksum", func(b *Batch) { b.Puts[0].PreviousSHA256 = "bad" }},
		{"record size", func(b *Batch) { b.Puts[0] = NewPut("state/one.json", nil, make([]byte, maxPutBytes+1)) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			test.change(&f.batch)
			if err := f.log.Commit(f.batch); !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("invalid input = %v", err)
			}
			if _, err := os.Stat(f.log.intentPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid intent published: %v", err)
			}
		})
	}
}

func TestBatchAggregateAndIndividualLimits(t *testing.T) {
	b := Batch{ID: "b"}
	for i := range 6 {
		b.Puts = append(b.Puts, NewPut(fmtBatchPath(i), nil, make([]byte, maxPutBytes)))
	}
	wide, err := EncodeJSON(b, 2*maxBatchBytes)
	if err != nil {
		t.Fatal(err)
	}
	shrink := 3 * ((len(wide)-maxBatchBytes)/4 + 1)
	b.Puts[5] = NewPut(b.Puts[5].Path, nil, make([]byte, maxPutBytes-shrink))
	fit, err := b.encode()
	if err != nil {
		t.Fatal(err)
	}
	b.ID += strings.Repeat("x", maxBatchBytes-len(fit))
	exact, err := b.encode()
	if err != nil || len(exact) != maxBatchBytes {
		t.Fatalf("exact aggregate limit: %d, %v", len(exact), err)
	}
	b.ID += "x"
	var size *SizeError
	if _, err := b.encode(); !errors.As(err, &size) || size.Size != maxBatchBytes+1 {
		t.Fatalf("aggregate overflow: %v", err)
	}
}

func fmtBatchPath(i int) string { return "outbox/record-" + string(rune('a'+i)) + ".json" }

func TestBatchOperationCountLimit(t *testing.T) {
	b := Batch{ID: "b"}
	for i := range maxBatchPuts {
		b.Puts = append(b.Puts, NewPut(fmtBatchPath(i), nil, nil))
	}
	if _, err := b.encode(); err != nil {
		t.Fatal(err)
	}
	b.Puts = append(b.Puts, NewPut("outbox/one-more.json", nil, nil))
	if _, err := b.encode(); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("count overflow = %v", err)
	}
}

func TestBatchDomainCheckRefusesBeforePublication(t *testing.T) {
	f := newBatchFixture(t)
	writeBatchIntent(t, f)
	if err := f.log.Recover(func(Batch) error { return errBatchCut }); !errors.Is(err, errBatchCut) {
		t.Fatal(err)
	}
	assertBatchTargets(t, f, false)
}

func TestBatchCorruptIntentIsNotApplied(t *testing.T) {
	for _, raw := range []string{`{`, `{"id":"b","puts":[]}`, `{"id":"b","puts":[],"extra":true}`} {
		t.Run(raw, func(t *testing.T) {
			f := newBatchFixture(t)
			if err := os.WriteFile(f.log.intentPath(), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err := f.log.Recover(nil); err == nil {
				t.Fatal("corrupt intent accepted")
			}
			assertBatchTargets(t, f, false)
		})
	}
}

func TestBatchPutSnapshotsInput(t *testing.T) {
	data := []byte("original")
	put := NewPut("state/one.json", nil, data)
	data[0] = 'x'
	if string(put.Data) != "original" || put.SHA256 != batchDigest(put.Data) {
		t.Fatal("put retained caller buffer")
	}
	if NewPut("state/one.json", []byte{}, nil).PreviousSHA256 == "" {
		t.Fatal("empty file confused with absent file")
	}
}

func TestBatchRejectsSymlinkParents(t *testing.T) {
	f := newBatchFixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(f.log.Root, "linked")); err != nil {
		t.Fatal(err)
	}
	f.batch.Puts[1] = NewPut("linked/event.json", nil, []byte("event"))
	if err := f.log.Commit(f.batch); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("symlink parent = %v", err)
	}
}

func TestBatchRejectsCorruptAfterImageBeforeAnyRedo(t *testing.T) {
	f := newBatchFixture(t)
	raw, err := json.Marshal(f.batch)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(f.batch.Puts[1].SHA256), []byte(strings.Repeat("0", 64)), 1)
	if err := os.WriteFile(f.log.intentPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.log.Recover(nil); !errors.Is(err, ErrInvalidBatch) {
		t.Fatal(err)
	}
	assertBatchTargets(t, f, false)
}
