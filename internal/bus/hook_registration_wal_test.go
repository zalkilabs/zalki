package bus

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPromptRegistrationWALRecoveryDoesNotRecharge(t *testing.T) {
	if dir := os.Getenv("ZALKI_PROMPT_WAL_STATE"); dir != "" {
		runPromptWALCrash(t, dir)
		return
	}
	for applied := range 4 {
		t.Run(string(rune('0'+applied)), func(t *testing.T) { assertPromptWALRecovery(t, applied) })
	}
}

func assertPromptWALRecovery(t *testing.T, applied int) {
	t.Helper()
	store := fixtureStore(t)
	native := newID()
	generation := newID()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPromptRegistrationWALRecoveryDoesNotRecharge$", "-test.count=1")
	child.Env = append(os.Environ(), "ZALKI_PROMPT_WAL_STATE="+store.Dir, "ZALKI_PROMPT_WAL_NATIVE="+native, "ZALKI_PROMPT_WAL_GENERATION="+generation, "ZALKI_PROMPT_WAL_APPLIED="+strconv.Itoa(applied))
	err := child.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatal("C18_ACTUAL_REGISTRATION_WAL_CRASH", err)
	}
	registered := model.Session{ID: "cursor:" + native, Generation: generation}
	assertRecoveredPromptWAL(t, store, registered, native)
}

func runPromptWALCrash(t *testing.T, dir string) {
	t.Helper()
	store := Store{Dir: dir, Registry: testRegistry()}
	native := os.Getenv("ZALKI_PROMPT_WAL_NATIVE")
	applied, err := strconv.Atoi(os.Getenv("ZALKI_PROMPT_WAL_APPLIED"))
	if err != nil {
		t.Fatal(err)
	}
	err = store.locked(func() error {
		var err error
		registered, _, err := newRegistration(store, "cursor", native, "/workspace", nil, trustedPrompt(native))
		if err != nil {
			return err
		}
		if err := debitPromptRegistration(store.Dir, registered.ID, time.Now().UTC(), journal.WriteAtomic); err != nil {
			return err
		}
		registered.Generation = os.Getenv("ZALKI_PROMPT_WAL_GENERATION")
		batch := stagedPromptRegistration(t, store, registered)
		if len(batch.Puts) != 3 {
			t.Fatal("C18_EXISTING_THREE_IMAGE_WAL_UNCHANGED")
		}
		for _, put := range batch.Puts[:applied] {
			if err := journal.WriteAtomic(filepath.Join(store.Dir, put.Path), put.Data); err != nil {
				return err
			}
		}
		os.Exit(23)
		return nil
	})
	t.Fatal("C18_WAL_CRASH_POINT_NOT_REACHED", err)
}

func assertRecoveredPromptWAL(t *testing.T, store Store, registered model.Session, native string) {
	t.Helper()
	// An already committed prompt is recovered before even reading the quota.
	quotaPath := filepath.Join(store.Dir, "hook-registration-quota.json")
	const corrupt = "{quota became unreadable after the durable intent"
	if err := os.WriteFile(quotaPath, []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	host, err := prepareHookRuntime(store, "cursor", registered.ID, trustedPrompt(native))
	if err != nil || host.generation != registered.Generation {
		t.Fatal("C18_COMMITTED_REGISTRATION_RECOVERS_WITHOUT_RECHARGE", err)
	}
	if meta := eventState(t, store); meta.Count != 1 {
		t.Fatal("C18_RECOVERY_DID_NOT_REGISTER_TWICE", meta)
	}
	after, err := os.ReadFile(quotaPath)
	if err != nil || string(after) != corrupt {
		t.Fatal("C18_RECOVERY_DID_NOT_REWRITE_QUOTA", err)
	}
}

func stagedPromptRegistration(t *testing.T, store Store, next model.Session) journal.Batch {
	t.Helper()
	record, err := prepareSessionRecord(store, next)
	if err != nil {
		t.Fatal(err)
	}
	event, err := sessionEvent(nil, record.value, model.CauseRegister)
	if err != nil {
		t.Fatal(err)
	}
	writer := commitWriter{dir: store.Dir}
	batch, err := writer.prepare(nil, record.value, event)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureEventDirectories(store, orderEventDirectory, false); err != nil {
		t.Fatal(err)
	}
	stageCommit(t, commitFixture{writer: writer}, batch)
	return batch
}
