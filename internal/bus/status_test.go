package bus

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestDeliveryTransitionsPreserveCompletedAndUncertainAttempts(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, _ := registerPair(t, store, "codex", "cursor")
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(msg.ID, model.Receipt{Status: model.StatusUnknownTimeout}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "messages", msg.ID+".json")
	before, _ := os.ReadFile(path)
	if _, err := store.Finish(msg.ID, model.Receipt{Status: model.StatusSubmitted}); model.ErrorCodeOf(err) != model.CodeInvalidTransition {
		t.Fatal("uncertain submission became a success", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid transition mutated journal")
	}
	if err := store.Update(to.ID, model.SessionState("invented"), false); model.ErrorCodeOf(err) != model.CodeInvalidTransition {
		t.Fatal("unknown session state accepted", err)
	}
}
