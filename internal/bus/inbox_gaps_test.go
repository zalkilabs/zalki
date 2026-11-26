package bus

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// These tests read through Store.ReadMessages only. They name none of the
// private helpers of the inbox code, so they hold when those helpers move.
// A message from a plugin whose delivery is refused now is hidden from native
// reads (plugin_delivery_test.go); these tests pin the other side: when the
// delivery is allowed, the message is an ordinary inbox message.

func allowPluginDelivery(view JournalView, root string, _ model.Message) error {
	return view.WithWriteScope(root, func() error { return nil })
}

func requireOnlyMessage(t *testing.T, batch MessageBatch, err error, want string) {
	t.Helper()
	if err != nil || len(batch.Messages) != 1 || batch.Messages[0].ID != want {
		t.Fatalf("read = %d messages, %v; want only %s", len(batch.Messages), err, want)
	}
}

func TestAnAllowedPluginMessageIsListedByTheInboxAndHistoryScans(t *testing.T) {
	store, to, key, msg := pendingPluginFixture(t)
	store.pluginDelivery = allowPluginDelivery
	for _, history := range []bool{false, true} {
		batch, err := store.ReadMessages(MessageQuery{Actor: to.ID, Capability: key, History: history})
		requireOnlyMessage(t, batch, err, msg.ID)
	}
}

func TestAnAllowedPluginMessageCanBeReadByID(t *testing.T) {
	store, to, key, msg := pendingPluginFixture(t)
	store.pluginDelivery = allowPluginDelivery
	batch, err := store.ReadMessages(MessageQuery{Actor: to.ID, Capability: key, MessageID: msg.ID})
	requireOnlyMessage(t, batch, err, msg.ID)
}
