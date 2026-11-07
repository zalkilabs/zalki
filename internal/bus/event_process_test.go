package bus

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
)

func runEventChild(t *testing.T, dir, id string) {
	t.Helper()
	store := Store{Dir: dir, Registry: testRegistry()}
	for range 10 {
		if err := store.Update(id, StateReady, false); err != nil {
			t.Fatal(err)
		}
		if err := store.Update(id, StateBusy, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEventSequenceTwoProcesses(t *testing.T) {
	if dir := os.Getenv("ZALKI_EVENT_TEST_STATE"); dir != "" {
		runEventChild(t, dir, os.Getenv("ZALKI_EVENT_TEST_ID"))
		return
	}
	store := fixtureStore(t)
	one, _ := register(t, store, "codex")
	two, _ := register(t, store, "codex")
	fd, err := journal.OpenLockFile(filepath.Join(store.Dir, "lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.CloseLockFile(fd)
	if err := journal.LockExclusive(fd, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	var children []*exec.Cmd
	defer func() {
		cancel()
		for _, child := range children {
			if child.ProcessState == nil {
				_ = child.Wait()
			}
		}
	}()
	var logs [2]bytes.Buffer
	for i, session := range []Session{one, two} {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEventSequenceTwoProcesses$", "-test.count=1")
		cmd.Env = append(os.Environ(), "ZALKI_EVENT_TEST_STATE="+store.Dir, "ZALKI_EVENT_TEST_ID="+session.ID)
		cmd.Stdout, cmd.Stderr, cmd.WaitDelay = &logs[i], &logs[i], time.Second
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, cmd)
	}
	if err := journal.Unlock(fd); err != nil {
		t.Fatal(err)
	}
	for i, child := range children {
		if err := child.Wait(); err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, &logs[i])
		}
	}
	if metadata := eventState(t, store); metadata.Last != 20 || metadata.Count != 20 {
		t.Fatalf("concurrent metadata = %+v", metadata)
	}
	assertDistinctEventIDs(t, store, 20)
}

func assertDistinctEventIDs(t *testing.T, store Store, count uint64) {
	t.Helper()
	seen := make(map[string]bool)
	for sequence := uint64(1); sequence <= count; sequence++ {
		raw := readCommitFile(t, filepath.Join(store.Dir, eventRecordPath(sequence)))
		var record eventRecord
		if err := decodeEventJSON(raw, &record); err != nil || record.Sequence != sequence || seen[record.Event.ID] {
			t.Fatalf("invalid/duplicate record at %d: %+v, %v", sequence, record, err)
		}
		seen[record.Event.ID] = true
	}
}
