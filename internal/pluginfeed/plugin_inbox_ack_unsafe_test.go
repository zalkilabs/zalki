package pluginfeed

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// A plugin acknowledges a message of its own inbox. A message id that is not a
// canonical id, and a record that is not a safe regular file, both answer
// "not found": the caller never sees the file error itself.
func TestPluginInboxAckAnswersNotFoundForUnsafeRecords(t *testing.T) {
	x := newInboxReadFixture(t, "")
	ack := func(id string) error {
		return x.f.write(func(view bus.JournalView) error {
			return view.AckPluginInboxMessage(x.f.store.Dir(), x.status.ID, id)
		})
	}
	requireNotFound := func(t *testing.T, err error) {
		t.Helper()
		if model.ErrorCodeOf(err) != model.CodeNotFound || err.Error() != "not_found: plugin inbox message not found" {
			t.Fatalf("ack = %v, want the not_found refusal", err)
		}
	}
	t.Run("id is not canonical", func(t *testing.T) {
		requireNotFound(t, ack("not-an-id"))
	})
	t.Run("record is a symbolic link", func(t *testing.T) {
		path := filepath.Join(x.f.store.Dir(), "messages", x.message.ID+".json")
		target := filepath.Join(t.TempDir(), "target.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		requireNotFound(t, ack(x.message.ID))
	})
}
