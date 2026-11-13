package bus

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Inject real filesystem operation errors through the existing private quota
// writer seam. This tests admission/accounting, not a replacement production
// atomic-write implementation. Production still uses journal.WriteAtomic.
func TestPromptQuotaAtomicFailureAccounting(t *testing.T) {
	for _, cut := range []string{"temporary-write", "rename", "directory-sync"} {
		t.Run(cut, func(t *testing.T) { assertPromptQuotaAtomicFailure(t, cut) })
	}
}

func assertPromptQuotaAtomicFailure(t *testing.T, cut string) {
	t.Helper()
	store := fixtureStore(t)
	event := trustedPrompt(newID())
	actor := "cursor:" + event.SessionID
	err := store.locked(func() error {
		_, err := registerPromptAbsent(store, "cursor", actor, event, time.Now().UTC(), func(path string, raw []byte) error { return failPromptQuotaAtomicAt(path, raw, cut) })
		return err
	})
	if err == nil {
		t.Fatal("C18_REAL_ATOMIC_ERROR_PREVENTS_REGISTRATION", cut)
	}
	if _, err := store.session(actor); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("C18_NO_SESSION_AFTER_ATOMIC_FAILURE", cut, err)
	}
	path := filepath.Join(store.Dir, "hook-registration-quota.json")
	if cut == "directory-sync" {
		quota, err := readPromptQuota(path)
		if err != nil || len(quota.Attempts) != 1 || quota.Attempts[0].Actor != actor {
			t.Fatal("C18_RENAMED_DEBIT_REMAINS_SPENT_ON_SYNC_FAILURE", err)
		}
		return
	}
	if cut == "temporary-write" {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("C18_TEMP_WRITE_FAILURE_PUBLISHES_NO_DEBIT", err)
		}
	}
}

func failPromptQuotaAtomicAt(path string, raw []byte, cut string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".prompt-quota-fixture-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if cut == "temporary-write" {
		if err := file.Close(); err != nil {
			return err
		}
		_, err := file.Write(raw)
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if cut == "rename" {
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		return os.Rename(file.Name(), path)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	return dir.Sync()
}
