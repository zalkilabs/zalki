package bus

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func requireAbsentLock(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe created a missing lifetime lock %s: %v", path, err)
	}
}

func TestMissingListenerProbesDoNotCreateLockFiles(t *testing.T) {
	for _, operation := range []string{"liveness", "status", "preview", "remove", "prune preview", "prune"} {
		t.Run(operation, func(t *testing.T) {
			store := fixtureStore(t)
			v, _ := register(t, store, "cursor")
			path := store.listenerLockPath(v.Kind, v.ID)
			requireAbsentLock(t, path)
			switch operation {
			case "liveness":
				if store.lockHeld(v) {
					t.Fatal("missing lock reported a live listener")
				}
			case "status":
				if got := store.listenerStatus(v); got != model.ListenerNone {
					t.Fatalf("missing lock status = %q, want none", got)
				}
			case "preview", "remove":
				if err := store.locked(func() error {
					found, err := store.removeFreeListenerLock(v.Kind, v.ID, operation == "remove")
					if found {
						t.Error("missing lock was reported as a cleanup candidate")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "prune preview", "prune":
				// Force the retired-session cleanup path to probe a missing lock.
				v.State, v.Retired, v.UpdatedAt = model.StateRetired, true, "2000-01-01T00:00:00Z"
				if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
					t.Fatal(err)
				}
				report, err := store.Prune(operation == "prune preview", nil)
				if err != nil || len(report.DeletedSessions) != 1 || len(report.DeletedLocks) != 0 {
					t.Fatalf("missing-lock prune: %+v, %v", report, err)
				}
			}
			requireAbsentLock(t, path)
		})
	}
}

func TestJournalFailureReleasesExclusiveLock(t *testing.T) {
	for _, failure := range []string{"callback", "compatibility", "recovery"} {
		t.Run(failure, func(t *testing.T) {
			store := fixtureStore(t)
			// Prepare the root independently so a broken first transaction cannot
			// leave a lock that hides this test behind a second blocking acquire.
			if err := os.MkdirAll(store.Dir, 0700); err != nil {
				t.Fatal(err)
			}
			callbackError := errors.New("baseline callback failure")
			if failure == "compatibility" {
				futureStateMarker(t, store)
			}
			if failure == "recovery" {
				dir := filepath.Join(store.Dir, "transactions")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "11111111-1111-4111-8111-111111111111.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err := store.locked(func() error { called = true; return callbackError })
			if err == nil || called != (failure == "callback") {
				t.Fatalf("%s failure: callback=%v, error=%v", failure, called, err)
			}
			if failure == "callback" && err != callbackError { //nolint:errorlint // Characterize exact callback error identity, including absence of wrapping.
				t.Fatalf("callback error changed: %v", err)
			}
			if failure == "compatibility" && (model.ErrorCodeOf(err) != model.CodeUnsupportedSchema || err.Error() != "unsupported_schema: state formats are unsupported or unreadable; upgrade zalki before writing") {
				t.Fatalf("compatibility error changed: %v", err)
			}
			assertRawLockState(t, filepath.Join(store.Dir, "lock"), false)
		})
	}
}

func TestFailedRecordPublicationKeepsConservativeFormatMarker(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "cursor")
	path, _ := store.sessionPath(session.ID)
	session.Listener = &model.ListenerLease{ID: newID(), Generation: session.Generation}
	err := store.locked(func() error {
		if err := os.Remove(filepath.Join(store.Dir, stateFormatFile)); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.Remove(filepath.Dir(path)); err != nil {
			return err
		}
		return store.saveSession(session)
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing record parent must fail publication: %v", err)
	}
	requireAbsentLock(t, path)
	marker, err := os.ReadFile(filepath.Join(store.Dir, "state-formats.json"))
	if err != nil || string(marker) != "{\n  \"schema\": 1,\n  \"formats\": {\n    \"sessions\": 2\n  }\n}\n" {
		t.Fatalf("failed publication lost conservative high-water marker: %s, %v", marker, err)
	}
}
