package bus

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestEventAbsenceBarrierPrecedesLegacyCallbacks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission refusal does not apply to root")
	}
	for _, operation := range []string{"read", "same-state write"} {
		t.Run(operation, func(t *testing.T) {
			assertAbsenceBarrier(t, operation)
		})
	}
}

func assertAbsenceBarrier(t *testing.T, operation string) {
	t.Helper()
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.Dir, "commits")
	before := journalSnapshot(t, store)
	if err := os.Chmod(dir, 0300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if operation == "read" {
		if _, err := store.List(); err == nil {
			t.Fatal("read bypassed failed absence sync")
		}
	} else if err := store.Update(session.ID, model.StateBusy, false); err == nil {
		t.Fatal("same-state rewrite bypassed failed absence sync")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if after := journalSnapshot(t, store); !reflect.DeepEqual(before, after) {
		t.Fatal("legacy callback ran before absence was durable")
	}
}
