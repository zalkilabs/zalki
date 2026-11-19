package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestFeedMaxValidRecordsBoundWorkAndReleaseForNativeOperation(t *testing.T) {
	store, first, _, _ := memberCacheFixture(t)
	ids := prepareMaxValidTargetRecords(t, store, first)
	count := &memberWorkCount{limit: 4 << 20}
	started := make(chan struct{})
	done := make(chan error, 1)
	once := false
	var result []byte
	err := WithJournalWrite(store, func(view JournalView) error {
		charge := func(input, reads, native, tokens, probes int) error {
			if !once && input > 0 {
				once = true
				go func() { close(started); done <- store.Update(first.ID, model.StateBusy, false) }()
				<-started
			}
			return count.charge(input, reads, native, tokens, probes)
		}
		var err error
		result, err = PluginMembers(t.Context(), view, "first", "all", true, ids, charge)
		return err
	})
	if !once {
		t.Fatal("F_WORK_MAX_VALID_RECORDS_EXACT_PRE_READ_BOUND")
	}
	nativeErr := <-done
	assertMaxValidTargetBound(t, result, err, count)
	if nativeErr != nil {
		t.Fatal("F_WORK_BOUNDED_REFUSAL_RELEASES_FOR_COMPETING_NATIVE_OPERATION", nativeErr)
	}
	var session model.Session
	path, _ := store.sessionPath(first.ID)
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &session) != nil || session.State != model.StateBusy {
		t.Fatal("F_WORK_COMPETING_NATIVE_OPERATION_PUBLISHED", err)
	}
}

func prepareMaxValidTargetRecords(t *testing.T, store Store, first model.Session) []string {
	t.Helper()
	ids := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		record := first
		if i != 0 {
			record, _ = register(t, store, "fixture")
		}
		ids = append(ids, record.ID)
		raw, err := encodeStateJSON(record)
		if err != nil || len(raw) > maxStateFileBytes {
			t.Fatal(err)
		}
		raw = append(raw, bytes.Repeat([]byte(" "), maxStateFileBytes-len(raw))...)
		path, _ := store.sessionPath(record.ID)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func assertMaxValidTargetBound(t *testing.T, result []byte, err error, count *memberWorkCount) {
	t.Helper()
	if err == nil || len(result) != 0 || count.input != 4<<20 || count.reads != 32 || count.native != 33 || count.probes != 32 {
		t.Fatal("F_WORK_MAX_VALID_RECORDS_EXACT_PRE_READ_BOUND", count, err)
	}
}
