package bus

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestMessageScanCallersKeepTheirUnreadableRecordPolicies(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, toKey := registerPair(t, store, "codex", "cursor")
	if err := store.Update(to.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	msg, err := (&Service{Store: store}).Send(context.Background(), SendRequest{
		Actor: from.ID, Capability: key, To: to.ID, Body: "held fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	unreadable := newID()
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", unreadable+".json"), []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	// The inbox list skips and counts the record (message_unreadable_list_test.go).
	if batch, err := store.ReadMessages(MessageQuery{Actor: to.ID, Capability: toKey}); err != nil || batch.Unreadable != 1 {
		t.Fatal("inbox list failed on, or did not count, an unreadable record", err)
	}
	requireHookSkipsWhileHeld(t, store, to.ID, msg.ID)
	if err := store.locked(func() error {
		if _, _, err := store.queuedApprovalsRead(to.ID, false); err == nil {
			t.Fatal("strict approval read silently skipped unreadable record")
		}
		queued, _, err := store.queuedApprovalsRead(to.ID, true)
		if err != nil || len(queued) != 1 || queued[0].ID != msg.ID {
			t.Fatal("prune lost readable held message", err)
		}
		counts, broken, err := store.approvalHolds()
		if err != nil || counts[to.ID] != 1 || len(broken) != 1 || broken[0] != unreadable {
			t.Fatal("recount lost counts or unreadable ID", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// requireHookSkipsWhileHeld checks that hook delivery skips and reports the
// record (message_unreadable_hook_test.go) and that the held message stays held
// while its prompt is open.
func requireHookSkipsWhileHeld(t *testing.T, store Store, target, held string) {
	t.Helper()
	text, err := store.EmitPending(target)
	if err != nil || text != "" {
		t.Fatal("hook failed on an unreadable record", text, err)
	}
	if !storedMessage(t, store, held).Held {
		t.Fatal("hook released a held message while its prompt is open")
	}
}
